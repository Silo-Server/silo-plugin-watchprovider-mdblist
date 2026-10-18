package provider

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

const (
	kindFavorite   = pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_FAVORITE
	addFavorite    = pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_ADD_FAVORITE
	removeFavorite = pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_REMOVE_FAVORITE
)

// fakeLists is an MDBList account's lists. It answers /lists/user from lists,
// which a create replaces with afterCreate, answers a create with nextID or,
// when set, the createStatus error, and records every request as
// "METHOD path" plus the body when one was sent.
type fakeLists struct {
	t            *testing.T
	mu           sync.Mutex
	lists        string
	afterCreate  string
	nextID       string
	createStatus int
	items        map[string]string
	requests     []string
}

func (f *fakeLists) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	request := r.Method + " " + r.URL.Path
	if len(body) > 0 {
		request += " " + string(body)
	}
	f.requests = append(f.requests, request)
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/lists/user":
		writeJSON(w, f.lists)
	case r.Method == http.MethodPost && r.URL.Path == "/lists/user/add":
		if f.afterCreate != "" {
			f.lists = f.afterCreate
		}
		if f.createStatus != 0 {
			w.WriteHeader(f.createStatus)
			return
		}
		writeJSON(w, `{"id":`+f.nextID+`,"name":"Silo Favorites","slug":"silo-favorites"}`)
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/items"):
		page, ok := f.items[r.URL.Path+"?"+r.URL.Query().Get("cursor")]
		if !ok {
			f.t.Errorf("unexpected read %s?%s", r.URL.Path, r.URL.RawQuery)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, page)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/lists/"):
		writeJSON(w, `{"added":{"movies":1,"shows":1},"removed":{"movies":0,"shows":0},"existing":{"movies":0,"shows":0},"not_found":{"movies":0,"shows":0}}`)
	default:
		f.t.Errorf("unexpected request %s", request)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeLists) assertRequests(want ...string) {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Join(f.requests, "\n") != strings.Join(want, "\n") {
		f.t.Fatalf("requests:\n%s\nwant:\n%s", strings.Join(f.requests, "\n"), strings.Join(want, "\n"))
	}
}

// favoritesOn is a connection with Sync favorites turned on.
func favoritesOn() *pluginv1.WatchSyncAuthenticatedContext {
	auth := authContext(testKey)
	auth.ConnectionSettings = map[string]string{settingSyncFavorites: "true"}
	return auth
}

func applyFavorites(t *testing.T, s *Server, events ...*pluginv1.WatchSyncEvent) *pluginv1.WatchSyncApplyEventsResponse {
	t.Helper()
	return applyEventsWithContext(t, s, favoritesOn(), events...)
}

func listFavoritesOn(t *testing.T, s *Server) traversal {
	t.Helper()
	return listAllWithContext(t, s, kindFavorite, favoritesOn())
}

func TestFavoritesStayOffUntilTheConnectionTurnsThemOn(t *testing.T) {
	s := newTestServer(t, func(http.ResponseWriter, *http.Request) {
		t.Error("MDBList should not be called while Sync favorites is off")
	})
	// Hosts send every declared setting; an older one sends none.
	off := authContext(testKey)
	off.ConnectionSettings = map[string]string{settingSyncFavorites: "false"}
	for name, auth := range map[string]*pluginv1.WatchSyncAuthenticatedContext{"off": off, "absent": authContext(testKey)} {
		result := listAllWithContext(t, s, kindFavorite, auth)
		if result.fault != nil || len(result.items) != 0 || result.complete {
			t.Fatalf("%s: traversal = %+v, want an empty, incomplete read", name, result)
		}
		response := applyEventsWithContext(t, s, auth,
			movieEvent("m1", addFavorite, map[string]string{"tmdb": "278"}),
			movieEvent("m2", removeFavorite, map[string]string{"tmdb": "238"}),
		)
		for _, id := range []string{"m1", "m2"} {
			// A retry keeps the explanation; Silo reports a rejected
			// favorite as not found instead.
			result := assertStatus(t, response, id, statusRetry)
			if result.GetFault().GetSafeMessage() != favoritesOffMessage {
				t.Fatalf("%s: %s fault = %v", name, id, result.GetFault())
			}
		}
	}
}

func newFakeLists(t *testing.T, lists string) (*fakeLists, *Server) {
	f := &fakeLists{t: t, lists: lists, nextID: "12", items: map[string]string{}}
	return f, newTestServer(t, f.handler)
}

func TestListFavoritesWithoutTheListImportsNothingAndRemovesNothing(t *testing.T) {
	f, s := newFakeLists(t, `[{"id":3,"name":"Watch later","dynamic":false}]`)
	result := listFavoritesOn(t, s)
	if result.fault != nil || len(result.items) != 0 || result.complete {
		t.Fatalf("traversal = %+v, want an empty, incomplete read", result)
	}
	f.assertRequests("GET /lists/user")
}

func TestListFavoritesReadsTheOldestSiloFavoritesListAcrossPages(t *testing.T) {
	f, s := newFakeLists(t, `[
		{"id":2,"name":"Silo Favorites","dynamic":true},
		{"id":9,"name":"Silo Favorites","dynamic":false},
		{"id":5,"name":" silo favorites ","dynamic":false},
		{"id":4,"name":"Favorites","dynamic":false}
	]`)
	f.items["/lists/5/items?"] = `{"movies":[{"id":917496,"title":"Beetlejuice Beetlejuice","imdb_id":"tt2049403","mediatype":"movie","release_year":2024}],"shows":[],"pagination":{"next_cursor":"c2"}}`
	f.items["/lists/5/items?c2"] = `{"movies":[],"shows":[{"id":1396,"title":"Breaking Bad","tvdb_id":81189,"mediatype":"show","release_year":2008}],"pagination":{"has_more":false}}`
	result := listFavoritesOn(t, s)
	if result.fault != nil || !result.complete || len(result.items) != 2 {
		t.Fatalf("traversal = %+v, want two favorites in a complete read", result)
	}
	movie, show := result.items[0], result.items[1]
	if movie.GetProviderItemKey() != "imdb:tt2049403" || movie.GetMedia().GetExternalIds()["tmdb"] != "917496" ||
		movie.GetFavorite() == nil || movie.GetWatchlist() != nil {
		t.Fatalf("movie = %v", movie)
	}
	if show.GetProviderItemKey() != "tvdb:81189" || show.GetMedia().GetMediaType() != pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_SERIES ||
		show.GetFavorite() == nil || show.GetWatchlist() != nil {
		t.Fatalf("show = %v", show)
	}
	// The second page reads the list the first page found, without a lookup.
	f.assertRequests("GET /lists/user", "GET /lists/5/items", "GET /lists/5/items")
}

func TestListFavoritesRejectsATokenWithoutAList(t *testing.T) {
	s := newTestServer(t, func(http.ResponseWriter, *http.Request) {
		t.Error("MDBList should not be called")
	})
	token := encodePageToken(pageToken{Kind: tokenKindFavorites, Cursor: "c2"})
	response, _ := s.ListRemoteState(context.Background(), &pluginv1.WatchSyncListRemoteStateRequest{
		Context:    favoritesOn(),
		PageToken:  token,
		StateKinds: []pluginv1.WatchSyncRemoteStateKind{kindFavorite},
	})
	if response.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST {
		t.Fatalf("fault = %v, want INVALID_REQUEST", response.GetFault())
	}
}

func TestAddFavoriteWritesBareIDsToTheExistingList(t *testing.T) {
	f, s := newFakeLists(t, `[{"id":7,"name":"Silo Favorites"}]`)
	show := seriesEvent("s1", addFavorite, nil)
	show.ProviderItemKey = "tvdb:81189"
	response := applyFavorites(t, s, movieEvent("m1", addFavorite, map[string]string{"imdb": "tt0111161", "tmdb": "278"}), show)
	assertStatus(t, response, "m1", statusApplied)
	assertStatus(t, response, "s1", statusApplied)
	f.assertRequests("GET /lists/user", `POST /lists/7/items/add {"movies":[{"imdb":"tt0111161","tmdb":278}],"shows":[{"tvdb":81189}]}`)
}

func TestAddFavoriteCreatesAPrivateListWhenTheAccountHasNone(t *testing.T) {
	f, s := newFakeLists(t, `[]`)
	f.afterCreate = `[{"id":12,"name":"Silo Favorites"}]`
	response := applyFavorites(t, s, movieEvent("m1", addFavorite, map[string]string{"tmdb": "278"}))
	assertStatus(t, response, "m1", statusApplied)
	f.assertRequests(
		"GET /lists/user",
		`POST /lists/user/add {"name":"Silo Favorites","private":true}`,
		"GET /lists/user",
		`POST /lists/12/items/add {"movies":[{"tmdb":278}]}`,
	)
}

func TestAddFavoriteUsesTheOlderListWhenAnotherServerCreatedOneAtTheSameTime(t *testing.T) {
	// The new list (12) is left alone: a server whose lookup did not yet show
	// list 9 may already have written to it.
	f, s := newFakeLists(t, `[]`)
	f.afterCreate = `[{"id":12,"name":"Silo Favorites"},{"id":9,"name":"Silo Favorites"}]`
	response := applyFavorites(t, s, movieEvent("m1", addFavorite, map[string]string{"tmdb": "278"}))
	assertStatus(t, response, "m1", statusApplied)
	f.assertRequests(
		"GET /lists/user",
		`POST /lists/user/add {"name":"Silo Favorites","private":true}`,
		"GET /lists/user",
		`POST /lists/9/items/add {"movies":[{"tmdb":278}]}`,
	)
}

func TestAddFavoriteWritesToTheListWhenMDBListRefusesADuplicateName(t *testing.T) {
	f, s := newFakeLists(t, `[]`)
	f.createStatus = http.StatusConflict
	f.afterCreate = `[{"id":9,"name":"Silo Favorites"}]`
	response := applyFavorites(t, s, movieEvent("m1", addFavorite, map[string]string{"tmdb": "278"}))
	assertStatus(t, response, "m1", statusApplied)
	f.assertRequests(
		"GET /lists/user",
		`POST /lists/user/add {"name":"Silo Favorites","private":true}`,
		"GET /lists/user",
		`POST /lists/9/items/add {"movies":[{"tmdb":278}]}`,
	)
}

func TestAddFavoriteExplainsARefusedListCreation(t *testing.T) {
	// The lookup accepted the key, so a 403 from the create refuses the list
	// and must not end the call as a rejected key.
	for _, status := range []int{http.StatusUnprocessableEntity, http.StatusForbidden} {
		f, s := newFakeLists(t, `[]`)
		f.createStatus = status
		response := applyFavorites(t, s,
			movieEvent("m1", addFavorite, map[string]string{"tmdb": "278"}),
			seriesEvent("s1", addFavorite, map[string]string{"tvdb": "81189"}),
		)
		if response.GetFault() != nil {
			t.Fatalf("HTTP %d: call fault = %v, want per-event results", status, response.GetFault())
		}
		for _, id := range []string{"m1", "s1"} {
			result := assertStatus(t, response, id, statusRetry)
			if !strings.Contains(result.GetFault().GetSafeMessage(), "list limit") {
				t.Fatalf("HTTP %d: %s fault = %v, want a list limit explanation", status, id, result.GetFault())
			}
		}
		f.assertRequests("GET /lists/user", `POST /lists/user/add {"name":"Silo Favorites","private":true}`, "GET /lists/user")
	}
}

func TestAddFavoriteWritesToItsNewListWhenTheLookupDoesNotShowItYet(t *testing.T) {
	// The new list (12) is older than the only one listed (15), so every
	// later lookup picks it; it must not be deleted.
	f, s := newFakeLists(t, `[]`)
	f.afterCreate = `[{"id":15,"name":"Silo Favorites"}]`
	response := applyFavorites(t, s, movieEvent("m1", addFavorite, map[string]string{"tmdb": "278"}))
	assertStatus(t, response, "m1", statusApplied)
	f.assertRequests(
		"GET /lists/user",
		`POST /lists/user/add {"name":"Silo Favorites","private":true}`,
		"GET /lists/user",
		`POST /lists/12/items/add {"movies":[{"tmdb":278}]}`,
	)
}

func TestRemoveFavoriteRetriesWhenTheListLookupFails(t *testing.T) {
	// Silo counts a rejected removal as done, so a failed lookup must leave
	// the removal for a retry instead.
	s := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})
	response := applyFavorites(t, s, movieEvent("m1", removeFavorite, map[string]string{"tmdb": "278"}))
	assertStatus(t, response, "m1", statusRetry)
}

func TestRemoveFavoriteWithoutTheListCostsNoWrite(t *testing.T) {
	f, s := newFakeLists(t, `[]`)
	response := applyFavorites(t, s,
		movieEvent("m1", removeFavorite, map[string]string{"tmdb": "278"}),
		seriesEvent("s1", removeFavorite, map[string]string{"tvdb": "81189"}),
	)
	assertStatus(t, response, "m1", statusNoChange)
	assertStatus(t, response, "s1", statusNoChange)
	f.assertRequests("GET /lists/user")
}

func TestFavoriteGroupsInOneCallShareOneLookup(t *testing.T) {
	f, s := newFakeLists(t, `[{"id":7,"name":"Silo Favorites"}]`)
	response := applyFavorites(t, s,
		movieEvent("m1", addFavorite, map[string]string{"tmdb": "278"}),
		movieEvent("m2", removeFavorite, map[string]string{"tmdb": "238"}),
	)
	assertStatus(t, response, "m1", statusApplied)
	assertStatus(t, response, "m2", statusApplied)
	f.assertRequests(
		"GET /lists/user",
		`POST /lists/7/items/add {"movies":[{"tmdb":278}]}`,
		`POST /lists/7/items/remove {"movies":[{"tmdb":238}]}`,
	)
}

func TestFavoritesRejectAnEpisodeWithoutCallingMDBList(t *testing.T) {
	s := newTestServer(t, func(http.ResponseWriter, *http.Request) {
		t.Error("MDBList should not be called for an unsupported item")
	})
	response := applyFavorites(t, s, episodeEvent("e1", addFavorite, map[string]string{"tvdb": "349232"}, nil, 1, 1))
	result := assertStatus(t, response, "e1", statusRejected)
	if !strings.Contains(result.GetFault().GetSafeMessage(), "favorites") {
		t.Fatalf("fault = %v", result.GetFault())
	}
}

func TestFavoritesLookupRateLimitLeavesTheBatchForLater(t *testing.T) {
	s := newTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	response := applyFavorites(t, s, movieEvent("m1", addFavorite, map[string]string{"tmdb": "278"}))
	result := assertStatus(t, response, "m1", statusRetry)
	if result.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_RATE_LIMITED {
		t.Fatalf("fault = %v, want RATE_LIMITED", result.GetFault())
	}
}
