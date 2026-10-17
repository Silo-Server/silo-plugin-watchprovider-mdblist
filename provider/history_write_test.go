package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// writeSequenceServer answers history reads with an empty history and each
// write with the next of responses, recording every write body.
type writeSequenceServer struct {
	responses []string
	writes    []string
}

func (f *writeSequenceServer) handle(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, `{"movies":[],"episodes":[],"pagination":{"next_cursor":null}}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.writes = append(f.writes, string(body))
		if len(f.writes) > len(f.responses) {
			t.Errorf("unexpected write %d: %s", len(f.writes), body)
			writeJSON(w, `{}`)
			return
		}
		writeJSON(w, f.responses[len(f.writes)-1])
	}
}

func TestMarkWatchedGroupsSeriesEpisodesByShowAndSeason(t *testing.T) {
	fake := &writeSequenceServer{responses: []string{`{"updated":{"episodes":5}}`}}
	s := newTestServer(t, fake.handle(t))
	day := time.Date(2020, 11, 4, 14, 0, 0, 0, time.UTC)
	showA := map[string]string{"tvdb": "254243"}
	showB := map[string]string{"tmdb": "1396"}
	response := applyEvents(t, s,
		at(episodeEvent("a-s1e1", markWatched, nil, showA, 1, 1), day),
		at(episodeEvent("a-s1e2", markWatched, nil, showA, 1, 2), day.Add(time.Minute)),
		at(episodeEvent("b-s0e3", markWatched, nil, showB, 0, 3), day.Add(2*time.Minute)),
		at(episodeEvent("a-s2e1", markWatched, nil, showA, 2, 1), day.Add(3*time.Minute)),
		// A rewatch is a second play of the same episode.
		at(episodeEvent("a-s1e1-again", markWatched, nil, showA, 1, 1), day.Add(4*time.Minute)),
	)
	for _, id := range []string{"a-s1e1", "a-s1e2", "b-s0e3", "a-s2e1", "a-s1e1-again"} {
		assertStatus(t, response, id, statusApplied)
	}
	if len(fake.writes) != 1 {
		t.Fatalf("writes = %d, want one", len(fake.writes))
	}
	assertJSONEqual(t, []byte(fake.writes[0]), `{"shows":[
		{"ids":{"tvdb":254243},"seasons":[
			{"number":1,"episodes":[
				{"number":1,"watched_at":"2020-11-04T14:00:00Z"},
				{"number":2,"watched_at":"2020-11-04T14:01:00Z"},
				{"number":1,"watched_at":"2020-11-04T14:04:00Z"}]},
			{"number":2,"episodes":[{"number":1,"watched_at":"2020-11-04T14:03:00Z"}]}]},
		{"ids":{"tmdb":1396},"seasons":[
			{"number":0,"episodes":[{"number":3,"watched_at":"2020-11-04T14:02:00Z"}]}]}
	]}`)
}

// MDBList echoes each refused entry the way it was sent: a whole show, a
// season of a show, or one episode of a show by its numbers.
func TestMarkWatchedRejectsOnlyThePlaysMDBListNamesUnderTheirShow(t *testing.T) {
	fake := &writeSequenceServer{responses: []string{`{
		"updated":{"movies":0,"shows":0,"seasons":0,"episodes":1},
		"not_found":{
			"movies":[],
			"shows":[{"ids":{"tvdb":1}}],
			"seasons":[{"number":5,"show":{"ids":{"tvdb":2}}}],
			"episodes":[{"season":1,"number":9,"show":{"ids":{"tvdb":3}}}]
		},
		"plays":[]
	}`}}
	s := newTestServer(t, fake.handle(t))
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	response := applyEvents(t, s,
		at(episodeEvent("unknown-show", markWatched, nil, map[string]string{"tvdb": "1"}, 1, 1), now),
		at(episodeEvent("unknown-season", markWatched, nil, map[string]string{"tvdb": "2"}, 5, 1), now),
		at(episodeEvent("known-season", markWatched, nil, map[string]string{"tvdb": "2"}, 4, 1), now),
		at(episodeEvent("unknown-episode", markWatched, nil, map[string]string{"tvdb": "3"}, 1, 9), now),
		at(episodeEvent("known-episode", markWatched, nil, map[string]string{"tvdb": "3"}, 1, 1), now),
	)
	for _, id := range []string{"unknown-show", "unknown-season", "unknown-episode"} {
		result := assertStatus(t, response, id, statusRejected)
		if result.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT {
			t.Fatalf("%s fault = %v", id, result.GetFault())
		}
	}
	assertStatus(t, response, "known-season", statusApplied)
	assertStatus(t, response, "known-episode", statusApplied)
}

// An error MDBList ties to one play retries that play alone. Its text stays
// out of the fault, which must not carry provider response bodies.
func TestMarkWatchedRetriesOnlyThePlayAnErrorNames(t *testing.T) {
	fake := &writeSequenceServer{responses: []string{`{
		"updated":{"movies":1,"episodes":0},
		"not_found":{"movies":[],"shows":[],"seasons":[],"episodes":[]},
		"errors":[{"type":"episode","ids":{"tmdb":62085},"error":"Episode could not be processed"}]
	}`}}
	s := newTestServer(t, fake.handle(t))
	response := applyEvents(t, s,
		at(episodeEvent("bad", markWatched, map[string]string{"tmdb": "62085"}, nil, 1, 1), time.Now()),
		at(movieEvent("good", markWatched, map[string]string{"imdb": "tt0111161"}), time.Now()),
	)
	result := assertStatus(t, response, "bad", statusRetry)
	if result.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY ||
		strings.Contains(result.GetFault().GetSafeMessage(), "could not be processed") {
		t.Fatalf("bad fault = %v, want a temporary fault without MDBList's text", result.GetFault())
	}
	assertStatus(t, response, "good", statusApplied)
}

func TestMarkWatchedRejectsAnEpisodeWithOnlyAnIMDbIDWithoutCallingMDBList(t *testing.T) {
	s := newTestServer(t, func(http.ResponseWriter, *http.Request) {
		t.Error("MDBList should not be called for an episode it cannot match")
	})
	response := applyEvents(t, s, at(episodeEvent("imdb-only", markWatched, map[string]string{"imdb": "tt0959621"}, nil, 1, 1), time.Now()))
	assertStatus(t, response, "imdb-only", statusRejected)
}

// This is MDBList's real answer to a flat episode without a TMDB or TVDB ID,
// the form the plugin used to send for series episodes: HTTP 200, nothing
// updated, and an error that names no play. No play is confirmed, so every
// play without another answer is retried instead of being reported applied.
func TestMarkWatchedRetriesPlaysWhenAnErrorNamesNoPlay(t *testing.T) {
	fake := &writeSequenceServer{responses: []string{`{
		"updated":{"movies":0,"shows":0,"seasons":0,"episodes":0},
		"history_only":{"movies":0,"episodes":0},
		"not_found":{"movies":[{"ids":{"imdb":"tt-missing"}}],"shows":[],"seasons":[],"episodes":[]},
		"errors":[{"type":"episode","ids":{},"error":"Episode ids missing; expected tmdb or tvdb id"}],
		"plays":[]
	}`}}
	s := newTestServer(t, fake.handle(t))
	response := applyEvents(t, s,
		at(movieEvent("missing", markWatched, map[string]string{"imdb": "tt-missing"}), time.Now()),
		at(movieEvent("unanswered", markWatched, map[string]string{"imdb": "tt0111161"}), time.Now()),
	)
	assertStatus(t, response, "missing", statusRejected)
	result := assertStatus(t, response, "unanswered", statusRetry)
	if result.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY {
		t.Fatalf("unanswered fault = %v", result.GetFault())
	}
}

func TestMarkWatchedWritesAnEpisodeMDBListCannotFindByIDUnderItsShow(t *testing.T) {
	fake := &writeSequenceServer{responses: []string{
		`{"not_found":{"episodes":[{"ids":{"tvdb":999}},{"ids":{"tvdb":998}}]}}`,
		`{"updated":{"episodes":1}}`,
	}}
	s := newTestServer(t, fake.handle(t))
	played := time.Date(2021, 3, 1, 20, 0, 0, 0, time.UTC)
	response := applyEvents(t, s,
		at(episodeEvent("has-show", markWatched, map[string]string{"tvdb": "999"}, map[string]string{"tvdb": "81189"}, 2, 3), played),
		at(episodeEvent("no-show", markWatched, map[string]string{"tvdb": "998"}, nil, 2, 4), played),
	)
	assertStatus(t, response, "has-show", statusApplied)
	assertStatus(t, response, "no-show", statusRejected)
	if len(fake.writes) != 2 {
		t.Fatalf("writes = %v, want the episode written again under its show", fake.writes)
	}
	assertJSONEqual(t, []byte(fake.writes[1]), `{"shows":[{"ids":{"tvdb":81189},"seasons":[{"number":2,"episodes":[{"number":3,"watched_at":"2021-03-01T20:00:00Z"}]}]}]}`)
}

func TestMarkWatchedSplitsWritesAtMDBListsShowLimit(t *testing.T) {
	fake := &writeSequenceServer{responses: []string{`{}`, `{}`}}
	s := newTestServer(t, fake.handle(t))
	events := make([]*pluginv1.WatchSyncEvent, 0, maxShowsPerWrite+1)
	for show := 1; show <= maxShowsPerWrite+1; show++ {
		events = append(events, at(episodeEvent(fmt.Sprintf("show-%d", show), markWatched, nil, map[string]string{"tvdb": strconv.Itoa(show)}, 1, 1), time.Now()))
	}
	response := applyEvents(t, s, events...)
	for _, event := range events {
		assertStatus(t, response, event.GetEventId(), statusApplied)
	}
	if len(fake.writes) != 2 {
		t.Fatalf("writes = %d, want two", len(fake.writes))
	}
	for index, want := range []int{maxShowsPerWrite, 1} {
		var payload mdblistWatchedPayload
		if err := json.Unmarshal([]byte(fake.writes[index]), &payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Shows) != want {
			t.Fatalf("write %d holds %d shows, want %d", index, len(payload.Shows), want)
		}
	}
}

func TestMarkUnwatchedRemovesSeriesEpisodesUnderTheirShow(t *testing.T) {
	fake := &writeSequenceServer{responses: []string{`{"removed":{"episodes":1}}`}}
	s := newTestServer(t, fake.handle(t))
	response := applyEvents(t, s, episodeEvent("u1", markUnwatched, nil, map[string]string{"tvdb": "254243"}, 4, 2))
	assertStatus(t, response, "u1", statusApplied)
	assertJSONEqual(t, []byte(fake.writes[0]), `{"shows":[{"ids":{"tvdb":254243},"seasons":[{"number":4,"episodes":[{"number":2}]}]}]}`)
}
