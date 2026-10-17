package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

const (
	// historyLookbackMargin widens the history read that checks a batch for
	// plays MDBList already holds. MDBList's since filter is documented only as
	// "items updated after", so the margin absorbs clock skew between Silo and
	// MDBList; the match itself compares exact seconds.
	historyLookbackMargin = 24 * time.Hour

	// maxHistoryCheckPages bounds that read. A recent batch fits in one page;
	// a backlog of old plays would otherwise cost a near-full history read per
	// batch against MDBList's daily quota, and MDBList's own folding of nearby
	// plays already keeps a write from adding a duplicate.
	maxHistoryCheckPages = 2

	// maxShowsPerWrite is MDBList's limit on show entries in one sync write.
	// MDBList rejects a whole write that exceeds it.
	maxShowsPerWrite = 200

	unsupportedPlayMessage = "MDBList watched sync requires a movie or episode with an external ID"
)

// playKind is where a play goes in a sync write.
type playKind int

const (
	// playMovie goes in movies[], matched by the movie's IDs.
	playMovie playKind = iota
	// playEpisodeByID goes in the flat episodes[], which MDBList matches only
	// by the episode's own TMDB or TVDB ID.
	playEpisodeByID
	// playEpisodeByShow goes in shows[].seasons[].episodes[], which MDBList
	// matches by the series' IDs and the season and episode numbers.
	playEpisodeByShow
)

// watchedPlay is one MARK_WATCHED or MARK_UNWATCHED event mapped to MDBList.
type watchedPlay struct {
	event *pluginv1.WatchSyncEvent
	kind  playKind
	// ids are the movie's or the episode's own IDs.
	ids     mdblistIDs
	showIDs mdblistIDs
	season  int
	number  int
	// sentAt is the watched_at a write sends, empty for an unwatched write.
	sentAt string
	// watchedAt is the play time in whole UTC seconds, or zero when the event
	// has none.
	watchedAt time.Time
	tokens    []string
}

// watchedPlayFromEvent maps an event to the write entry MDBList can match. An
// episode goes by its own TMDB or TVDB ID when it has one, and otherwise under
// its series by season and episode number. It reports false when MDBList
// could not identify the title.
func watchedPlayFromEvent(event *pluginv1.WatchSyncEvent, includeWatchedAt bool) (watchedPlay, bool) {
	media := event.GetMedia()
	play := watchedPlay{event: event}
	if occurred := event.GetOccurredAt(); occurred != nil && occurred.CheckValid() == nil && !occurred.AsTime().IsZero() {
		play.watchedAt = occurred.AsTime().UTC().Truncate(time.Second)
	}
	if includeWatchedAt && !play.watchedAt.IsZero() {
		play.sentAt = play.watchedAt.Format(time.RFC3339)
	}
	play.ids = idsFromMedia(media.GetExternalIds())
	switch media.GetMediaType() {
	case pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE:
		if play.ids == (mdblistIDs{}) {
			return watchedPlay{}, false
		}
		play.kind = playMovie
		play.tokens = movieTokens(play.ids, event.GetProviderItemKey())
	case pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE:
		play.showIDs = idsFromMedia(media.GetSeriesExternalIds())
		play.season, play.number = int(media.GetSeasonNumber()), int(media.GetEpisodeNumber())
		switch {
		case play.ids.TMDB > 0 || play.ids.TVDB > 0:
			play.kind = playEpisodeByID
		case play.canGoUnderShow():
			play.kind = playEpisodeByShow
		default:
			// MDBList cannot match an episode by its IMDb ID alone.
			return watchedPlay{}, false
		}
		play.tokens = episodeTokens(play.ids, play.showIDs, play.season, play.number, event.GetProviderItemKey())
	default:
		return watchedPlay{}, false
	}
	return play, true
}

// canGoUnderShow reports whether an episode can be written under its series.
func (p watchedPlay) canGoUnderShow() bool {
	return p.showIDs != (mdblistIDs{}) && p.number > 0
}

// watchedPlays maps events to plays, rejecting each event MDBList could not
// identify.
func watchedPlays(events []*pluginv1.WatchSyncEvent, results *resultSet, includeWatchedAt bool) []watchedPlay {
	plays := make([]watchedPlay, 0, len(events))
	for _, event := range events {
		play, ok := watchedPlayFromEvent(event, includeWatchedAt)
		if !ok {
			results.reject(event, unsupportedPlayMessage)
			continue
		}
		plays = append(plays, play)
	}
	return plays
}

// watchedWrites splits plays into writes of at most maxShowsPerWrite shows.
func watchedWrites(plays []watchedPlay) [][]watchedPlay {
	var writes [][]watchedPlay
	var current []watchedPlay
	shows := map[mdblistIDs]bool{}
	for _, play := range plays {
		if play.kind == playEpisodeByShow && !shows[play.showIDs] {
			if len(shows) == maxShowsPerWrite {
				writes = append(writes, current)
				current, shows = nil, map[mdblistIDs]bool{}
			}
			shows[play.showIDs] = true
		}
		current = append(current, play)
	}
	if len(current) > 0 {
		writes = append(writes, current)
	}
	return writes
}

func watchedPayload(plays []watchedPlay) mdblistWatchedPayload {
	var payload mdblistWatchedPayload
	showIndex := map[mdblistIDs]int{}
	for _, play := range plays {
		switch play.kind {
		case playMovie:
			payload.Movies = append(payload.Movies, mdblistWatchedMoviePayload{IDs: play.ids, WatchedAt: play.sentAt})
		case playEpisodeByID:
			payload.Episodes = append(payload.Episodes, mdblistWatchedEpisodePayload{IDs: play.ids, WatchedAt: play.sentAt})
		case playEpisodeByShow:
			index, ok := showIndex[play.showIDs]
			if !ok {
				index = len(payload.Shows)
				showIndex[play.showIDs] = index
				payload.Shows = append(payload.Shows, mdblistWatchedShowPayload{IDs: play.showIDs})
			}
			payload.Shows[index].add(play.season, play.number, play.sentAt)
		}
	}
	return payload
}

// markWatched adds plays to MDBList's history. Delivery is at least once, so
// it first reads the history around the batch's play times and answers a play
// MDBList already holds at the same second, the precision the built-in
// provider reconciled history at, with NO_CHANGE instead of writing it again.
// MDBList documents that it folds a written play into a nearby existing play
// instead of adding one, so a play the bounded read misses, or a batch whose
// read fails, is still written without creating a duplicate.
func (r *applyRun) markWatched(ctx context.Context, events []*pluginv1.WatchSyncEvent, results *resultSet) *pluginv1.WatchSyncFault {
	plays := watchedPlays(events, results, true)
	if len(plays) == 0 {
		return nil
	}
	held, fault := r.heldPlays(ctx, plays)
	if connectionWide(fault) {
		return fault
	}
	toSend := make([]watchedPlay, 0, len(plays))
	for _, play := range plays {
		if held.contains(play) {
			results.set(play.event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE)
			continue
		}
		toSend = append(toSend, play)
	}
	// A title MDBList does not know cannot be added; retrying would not change
	// that.
	notFound := func(play watchedPlay) {
		results.fail(play.event, permanentFault("MDBList could not find this title"))
	}
	return r.writeHistory(ctx, "/sync/watched", toSend, results, notFound)
}

// markUnwatched clears the watched state of each title. A title MDBList does
// not hold as watched is already in the desired state.
func (r *applyRun) markUnwatched(ctx context.Context, events []*pluginv1.WatchSyncEvent, results *resultSet) *pluginv1.WatchSyncFault {
	plays := watchedPlays(events, results, false)
	notFound := func(play watchedPlay) {
		results.set(play.event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE)
	}
	return r.writeHistory(ctx, "/sync/watched/remove", plays, results, notFound)
}

// writeHistory writes plays to path and answers each play from MDBList's
// response: a play MDBList names in not_found goes to notFound, a play it
// names in errors is retried, and every other play is
// applied. MDBList echoes each refused entry as it was sent, so one refused
// title does not hold back the rest of the write. An episode MDBList does not
// know by its own ID is written once more under its series. A refusal that
// names no play leaves that write's unanswered plays for a retry.
func (r *applyRun) writeHistory(
	ctx context.Context,
	path string,
	plays []watchedPlay,
	results *resultSet,
	notFound func(watchedPlay),
) *pluginv1.WatchSyncFault {
	for pass := 0; pass < 2 && len(plays) > 0; pass++ {
		var underShow []watchedPlay
		for _, write := range watchedWrites(plays) {
			if r.outOfTime() {
				failPlays(write, results, temporaryFault("MDBList sync time limit reached; the event will be retried"))
				continue
			}
			var response mdblistWriteResponse
			if err := r.client.post(ctx, path, watchedPayload(write), &response); err != nil {
				if fault := failPlays(write, results, faultFor(err)); fault != nil {
					return fault
				}
				continue
			}
			refused := response.refusals(write)
			for index, play := range write {
				switch {
				case refused.errors[index]:
					results.fail(play.event, temporaryFault("MDBList reported an error for this play; it will be retried"))
				case refused.notFound[index] && pass == 0 && play.kind == playEpisodeByID && play.canGoUnderShow():
					play.kind = playEpisodeByShow
					underShow = append(underShow, play)
				case refused.notFound[index]:
					notFound(play)
				case refused.unattributed:
					results.fail(play.event, temporaryFault("MDBList refused an item in the batch without naming it; the event will be retried"))
				default:
					results.set(play.event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED)
				}
			}
		}
		plays = underShow
	}
	return nil
}

func failPlays(plays []watchedPlay, results *resultSet, fault *pluginv1.WatchSyncFault) *pluginv1.WatchSyncFault {
	events := make([]*pluginv1.WatchSyncEvent, 0, len(plays))
	for _, play := range plays {
		events = append(events, play.event)
	}
	return failBatch(events, results, fault)
}

// historyRefusals is what one write's response refused, by the play's index
// in the write.
type historyRefusals struct {
	notFound map[int]bool
	errors   map[int]bool
	// unattributed is set when the response refused something it did not
	// name in a form that matches a play.
	unattributed bool
}

func (r mdblistWriteResponse) refusals(plays []watchedPlay) historyRefusals {
	out := historyRefusals{notFound: map[int]bool{}, errors: map[int]bool{}}
	if !emptyJSONValue(r.NotFound) {
		var buckets map[string]json.RawMessage
		if err := json.Unmarshal(r.NotFound, &buckets); err != nil {
			out.unattributed = true
		}
		for bucket, raw := range buckets {
			if emptyJSONValue(raw) {
				continue
			}
			var refs []mdblistHistoryRef
			if err := json.Unmarshal(raw, &refs); err != nil {
				out.unattributed = true
				continue
			}
			for _, ref := range refs {
				matched := ref.match(plays, strings.TrimSuffix(bucket, "s"))
				out.unattributed = out.unattributed || len(matched) == 0
				for _, index := range matched {
					out.notFound[index] = true
				}
			}
		}
	}
	if !emptyJSONValue(r.Errors) {
		var refs []mdblistHistoryRef
		if err := json.Unmarshal(r.Errors, &refs); err != nil {
			out.unattributed = true
		}
		for _, ref := range refs {
			matched := ref.match(plays, ref.Type)
			out.unattributed = out.unattributed || len(matched) == 0
			for _, index := range matched {
				out.errors[index] = true
			}
		}
	}
	return out
}

// match returns the indexes of the plays a refused entry of kind (movie,
// show, season, or episode) names.
func (ref mdblistHistoryRef) match(plays []watchedPlay, kind string) []int {
	var matched []int
	for index, play := range plays {
		if ref.names(play, kind) {
			matched = append(matched, index)
		}
	}
	return matched
}

func (ref mdblistHistoryRef) names(play watchedPlay, kind string) bool {
	switch kind {
	case "movie":
		return play.kind == playMovie && sharesID(play.ids, ref.IDs)
	case "show":
		showIDs := ref.IDs
		if ref.Show != nil && showIDs == (mdblistIDs{}) {
			showIDs = ref.Show.IDs
		}
		return play.kind == playEpisodeByShow && sharesID(play.showIDs, showIDs)
	case "season":
		return play.kind == playEpisodeByShow && ref.Show != nil &&
			sharesID(play.showIDs, ref.Show.IDs) && play.season == ref.Number
	case "episode":
		if ref.Show != nil {
			return play.kind == playEpisodeByShow && sharesID(play.showIDs, ref.Show.IDs) &&
				play.season == ref.Season && play.number == ref.Number
		}
		return play.kind == playEpisodeByID && sharesID(play.ids, ref.IDs)
	default:
		return false
	}
}

// sharesID reports whether two ID sets name the same title by any ID.
func sharesID(a, b mdblistIDs) bool {
	return (a.IMDb != "" && strings.EqualFold(a.IMDb, b.IMDb)) ||
		(a.TMDB > 0 && a.TMDB == b.TMDB) ||
		(a.TVDB > 0 && a.TVDB == b.TVDB) ||
		(a.Trakt > 0 && a.Trakt == b.Trakt) ||
		(a.MDBList != "" && a.MDBList == b.MDBList)
}

// playSet holds the identity tokens of plays MDBList holds, each at its play
// time in whole seconds.
type playSet map[string]struct{}

func playSetKey(token string, at time.Time) string {
	return token + "@" + strconv.FormatInt(at.Unix(), 10)
}

func (s playSet) add(tokens []string, at time.Time) {
	at = at.UTC().Truncate(time.Second)
	for _, token := range tokens {
		s[playSetKey(token, at)] = struct{}{}
	}
}

func (s playSet) contains(play watchedPlay) bool {
	if play.watchedAt.IsZero() {
		return false
	}
	for _, token := range play.tokens {
		if _, ok := s[playSetKey(token, play.watchedAt)]; ok {
			return true
		}
	}
	return false
}

// heldPlays reads MDBList's plays since the batch's earliest play time. The
// read stops after maxHistoryCheckPages pages, or when half the call's time box
// is spent; the plays read by then still count, and MDBList's own folding of
// nearby plays covers the rest. A failed read returns the plays read so far
// with the fault.
func (r *applyRun) heldPlays(ctx context.Context, plays []watchedPlay) (playSet, *pluginv1.WatchSyncFault) {
	held := playSet{}
	var earliest time.Time
	for _, play := range plays {
		if !play.watchedAt.IsZero() && (earliest.IsZero() || play.watchedAt.Before(earliest)) {
			earliest = play.watchedAt
		}
	}
	if earliest.IsZero() {
		return held, nil
	}
	since := earliest.Add(-historyLookbackMargin).Format(time.RFC3339)
	state := pageState{}
	for range maxHistoryCheckPages {
		if r.server.now().Sub(r.startedAt) >= r.budget/2 {
			return held, nil
		}
		// MDBList's cursors continue the result set they came from, and its
		// other cursor-paged reads refuse since next to cursor, so since goes
		// only on requests that start the set or page it by offset.
		extra := url.Values{"plays": {"all"}}
		if state.cursor == "" {
			extra.Set("since", since)
		}
		var payload mdblistWatchedResponse
		if err := r.client.get(ctx, "/sync/watched", state.query(extra), &payload); err != nil {
			return held, faultFor(err)
		}
		for _, movie := range payload.Movies {
			if movie.LastWatchedAt != nil {
				held.add(movieTokens(movie.Movie.IDs, movieKey(movie.Movie.IDs)), *movie.LastWatchedAt)
			}
		}
		for _, episode := range payload.Episodes {
			if episode.LastWatchedAt != nil {
				key := episodeKey(episode.Show.IDs, episode.Season, episode.Number, episode.IDs)
				held.add(episodeTokens(episode.IDs, episode.Show.IDs, episode.Season, episode.Number, key), *episode.LastWatchedAt)
			}
		}
		done, err := state.advance(payload.Pagination, payload.fetched())
		if err != nil {
			return held, temporaryFault(invalidPaginationPrefix + err.Error())
		}
		if done {
			return held, nil
		}
	}
	return held, nil
}

// movieTokens names a movie by each of its IDs and by its provider key, so a
// play matches whichever ID MDBList and Silo both know.
func movieTokens(ids mdblistIDs, providerItemKey string) []string {
	tokens := idTokens("movie|", ids)
	if key := strings.TrimSpace(providerItemKey); key != "" {
		tokens = append(tokens, "movie|"+key)
	}
	return tokens
}

// episodeTokens names an episode by each of its own IDs, by each of its
// series' IDs with season and episode numbers, and by its provider key. These
// are the forms episodeKey produces.
func episodeTokens(ids, showIDs mdblistIDs, season, episode int, providerItemKey string) []string {
	tokens := idTokens("episode|", ids)
	if showIDs.TVDB > 0 {
		tokens = append(tokens, fmt.Sprintf("episode|show:tvdb:%d:s%d:e%d", showIDs.TVDB, season, episode))
	}
	if showIDs.TMDB > 0 {
		tokens = append(tokens, fmt.Sprintf("episode|show:tmdb:%d:s%d:e%d", showIDs.TMDB, season, episode))
	}
	if showIDs.IMDb != "" {
		tokens = append(tokens, fmt.Sprintf("episode|show:imdb:%s:s%d:e%d", showIDs.IMDb, season, episode))
	}
	if key := strings.TrimSpace(providerItemKey); key != "" {
		tokens = append(tokens, "episode|"+key)
	}
	return tokens
}

func idTokens(prefix string, ids mdblistIDs) []string {
	var tokens []string
	if ids.IMDb != "" {
		tokens = append(tokens, prefix+"imdb:"+ids.IMDb)
	}
	if ids.TMDB > 0 {
		tokens = append(tokens, prefix+"tmdb:"+strconv.Itoa(ids.TMDB))
	}
	if ids.TVDB > 0 {
		tokens = append(tokens, prefix+"tvdb:"+strconv.Itoa(ids.TVDB))
	}
	return tokens
}
