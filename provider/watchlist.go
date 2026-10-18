package provider

import (
	"context"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// writeWatchlist adds or removes watchlist titles in one request.
//
// MDBList's API has no way to order the watchlist, so list_position is
// ignored.
func (c *apiClient) writeWatchlist(ctx context.Context, events []*pluginv1.WatchSyncEvent, results *resultSet, removing bool) *pluginv1.WatchSyncFault {
	titles := splitTitles(events, results, "watchlist")
	if len(titles.sent) == 0 {
		return nil
	}
	var payload mdblistListPayload
	for _, ids := range titles.movies {
		payload.Movies = append(payload.Movies, mdblistRef{IDs: ids})
	}
	for _, ids := range titles.shows {
		payload.Shows = append(payload.Shows, mdblistRef{IDs: ids})
	}
	path := "/watchlist/items/add"
	if removing {
		path = "/watchlist/items/remove"
	}
	return c.writeTitles(ctx, path, payload, titles.sent, results, removing, "watchlist")
}

// titleBatch is the movies and series of one list write, and the events they
// came from.
type titleBatch struct {
	movies []mdblistIDs
	shows  []mdblistIDs
	sent   []*pluginv1.WatchSyncEvent
}

// splitTitles sorts the events of a list write into movies and series. A
// title MDBList cannot identify is rejected and left out of the batch; list
// names the list in that rejection.
func splitTitles(events []*pluginv1.WatchSyncEvent, results *resultSet, list string) titleBatch {
	batch := titleBatch{sent: make([]*pluginv1.WatchSyncEvent, 0, len(events))}
	for _, event := range events {
		ids, ok := listItemIDs(event)
		if !ok {
			results.reject(event, "MDBList "+list+" sync requires a movie or series with an external ID")
			continue
		}
		if event.GetMedia().GetMediaType() == pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE {
			batch.movies = append(batch.movies, ids)
		} else {
			batch.shows = append(batch.shows, ids)
		}
		batch.sent = append(batch.sent, event)
	}
	return batch
}

// writeTitles sends one list write. MDBList answers with per-kind counts and
// never names the titles it did not accept:
//   - an add with any not_found entry confirms no title, so the batch is
//     retried;
//   - a removal with any not_found entry still leaves every title absent,
//     which is the desired state.
func (c *apiClient) writeTitles(ctx context.Context, path string, payload any, sent []*pluginv1.WatchSyncEvent, results *resultSet, removing bool, list string) *pluginv1.WatchSyncFault {
	var response mdblistWriteResponse
	if err := c.post(ctx, path, payload, &response); err != nil {
		return failBatch(sent, results, faultFor(err))
	}
	notFound := !emptyJSONValue(response.NotFound)
	for _, event := range sent {
		switch {
		case !notFound:
			results.set(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED)
		case removing:
			results.set(event, pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE)
		default:
			results.fail(event, temporaryFault("MDBList did not accept one or more "+list+" items in the batch"))
		}
	}
	return nil
}
