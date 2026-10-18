package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// favoritesListName names the MDBList list that holds Silo favorites. MDBList
// has no favorites of its own, so the plugin keeps them in a private static
// list of the account. Every Silo server connected to the account finds the
// list by this name, which keeps their favorites in step.
const favoritesListName = "Silo Favorites"

// settingSyncFavorites is the connection setting that turns favorites sync
// on. It is off by default because the first export creates a list in the
// MDBList account. Silo turns its own favorites switches on for every new
// connection, so they cannot keep the list from being created.
const settingSyncFavorites = "sync_favorites"

// favoritesOffMessage answers favorite writes while the setting is off. The
// host shows it with the sync run and sends the favorites again next sync,
// so they go out once the setting is turned on.
const favoritesOffMessage = "Turn on Sync favorites for this MDBList connection to send favorites to MDBList"

func favoritesEnabled(auth *pluginv1.WatchSyncAuthenticatedContext) bool {
	return auth.GetConnectionSettings()[settingSyncFavorites] == "true"
}

// findFavoritesList returns the ID of the account's Silo Favorites list, or 0
// when it has none. When two servers each created one, every server uses the
// oldest, the one with the lowest ID, so they all read and write one list.
// A dynamic list of that name is skipped: MDBList fills it from filters, and
// it takes no writes.
func (c *apiClient) findFavoritesList(ctx context.Context) (int, error) {
	var lists []mdblistUserList
	if err := c.get(ctx, "/lists/user", nil, &lists); err != nil {
		return 0, err
	}
	id := 0
	for _, list := range lists {
		if list.ID <= 0 || list.Dynamic || !strings.EqualFold(strings.TrimSpace(list.Name), favoritesListName) {
			continue
		}
		if id == 0 || list.ID < id {
			id = list.ID
		}
	}
	return id, nil
}

// createFavoritesList creates the private Silo Favorites list and returns the
// list every server should use. It looks the list up again after creating it:
// another server may have created one at the same moment, and the oldest one
// wins. A list this call created that lost is left in place: another server
// whose lookup did not yet show the older list may already have written to
// it, and deleting it would destroy those favorites in MDBList. A refused
// create also looks again, in case MDBList refused a duplicate name.
//
// Only a rate limit ends the call. The lookup before the create accepted the
// API key, so a 401 or 403 here refuses the list, not the key.
func (c *apiClient) createFavoritesList(ctx context.Context) (int, *pluginv1.WatchSyncFault) {
	var created mdblistUserList
	createErr := c.post(ctx, "/lists/user/add", mdblistCreateList{Name: favoritesListName, Private: true}, &created)
	var limited *rateLimitedError
	if errors.As(createErr, &limited) {
		return 0, faultFor(createErr)
	}
	id, err := c.findFavoritesList(ctx)
	if err != nil {
		return 0, faultFor(err)
	}
	if createErr != nil {
		if id == 0 {
			return 0, createListFault(createErr)
		}
		return id, nil
	}
	switch {
	case created.ID > 0 && (id == 0 || created.ID < id):
		// The lookup does not list the new list yet, but it is the oldest,
		// so the next lookup will pick it.
		return created.ID, nil
	case id == 0:
		return 0, temporaryFault("MDBList did not return the new " + favoritesListName + " list")
	}
	return id, nil
}

// createListFault explains a refused list creation. MDBList limits how many
// static lists an account may keep, and the response body that would say so
// cannot be quoted in a fault.
func createListFault(err error) *pluginv1.WatchSyncFault {
	var status *statusError
	if !errors.As(err, &status) || status.status >= http.StatusInternalServerError {
		return faultFor(err)
	}
	return permanentFault(fmt.Sprintf(
		"MDBList refused to create the %s list (HTTP %d); the account may have reached its list limit",
		favoritesListName, status.status))
}

// favoritesList returns the Silo Favorites list for this call, creating it
// when create is set and the account has none. It returns 0 when the account
// has no list and create is not set. The ID is kept for the rest of the call,
// so adds and removals in one call cost one lookup.
func (r *applyRun) favoritesList(ctx context.Context, create bool) (int, *pluginv1.WatchSyncFault) {
	if !r.favoritesListFound {
		id, err := r.client.findFavoritesList(ctx)
		if err != nil {
			return 0, faultFor(err)
		}
		r.favoritesListID, r.favoritesListFound = id, true
	}
	if r.favoritesListID == 0 && create {
		id, fault := r.client.createFavoritesList(ctx)
		if fault != nil {
			return 0, fault
		}
		r.favoritesListID = id
	}
	return r.favoritesListID, nil
}

// writeFavorites adds or removes favorites in one write to the Silo Favorites
// list. The first add creates the list. A removal from an account without the
// list has nothing to remove, so it costs no write. While Sync favorites is
// off, nothing is written and every event is left for a later sync.
func (r *applyRun) writeFavorites(ctx context.Context, events []*pluginv1.WatchSyncEvent, results *resultSet, removing bool) *pluginv1.WatchSyncFault {
	if !r.favoritesEnabled {
		// A retry, not a rejection: Silo reports a rejected favorite as not
		// found and drops this message.
		for _, event := range events {
			results.fail(event, temporaryFault(favoritesOffMessage))
		}
		return nil
	}
	titles := splitTitles(events, results, "favorites")
	if len(titles.sent) == 0 {
		return nil
	}
	listID, fault := r.favoritesList(ctx, !removing)
	if fault != nil {
		// Finding or creating the list concerns no single title, so every
		// event is retried with the fault's message rather than failBatch's.
		// Never a rejection: Silo counts a rejected removal as done, and the
		// next read would bring the favorite back.
		if connectionWide(fault) {
			return fault
		}
		for _, event := range titles.sent {
			results.fail(event, temporaryFault(fault.GetSafeMessage()))
		}
		return nil
	}
	if listID == 0 {
		for _, event := range titles.sent {
			results.set(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE)
		}
		return nil
	}
	action := "add"
	if removing {
		action = "remove"
	}
	path := "/lists/" + strconv.Itoa(listID) + "/items/" + action
	payload := mdblistStaticListPayload{Movies: titles.movies, Shows: titles.shows}
	return r.client.writeTitles(ctx, path, payload, titles.sent, results, removing, "favorites")
}

// listFavorites returns one page of the Silo Favorites list, or nothing while
// Sync favorites is off. The first page finds the list, and the page token
// carries its ID so every page reads the same list. An account without the
// list has no favorites to import, but that read is not a complete snapshot:
// the list may be gone because someone deleted or renamed it, and that must
// not remove every favorite in Silo. The same goes for a read with the
// setting off.
func listFavorites(ctx context.Context, client *apiClient, rawToken string, enabled bool) *pluginv1.WatchSyncListRemoteStateResponse {
	if !enabled {
		return &pluginv1.WatchSyncListRemoteStateResponse{}
	}
	token, fault := decodePageToken(rawToken, tokenKindFavorites)
	if fault != nil {
		return listFault(fault)
	}
	if token.List == 0 {
		id, err := client.findFavoritesList(ctx)
		if err != nil {
			return listFault(faultFor(err))
		}
		if id == 0 {
			return &pluginv1.WatchSyncListRemoteStateResponse{}
		}
		token.List = id
	}
	return listTitles(ctx, client, "/lists/"+strconv.Itoa(token.List)+"/items", token, favoriteState)
}

func favoriteState(state *pluginv1.WatchSyncRemoteState) {
	state.Favorite = &pluginv1.WatchSyncRemoteListState{}
}
