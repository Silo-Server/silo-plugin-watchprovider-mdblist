# MDBList watch-provider plugin for Silo

Syncs Silo profiles with an [MDBList](https://mdblist.com) account through Silo's `watch_sync_provider.v1` plugin contract. It replaces the MDBList provider built into earlier Silo releases; the differences are listed under [Upgrading from the built-in provider](#upgrading-from-the-built-in-provider).

## Capabilities

- Imports movie and episode watch history, one entry per play.
- Exports completed movie and episode watches, and marks titles unwatched.
- Imports resume progress from paused MDBList playback sessions.
- Imports, exports, and removes watchlist movies and series, and keeps MDBList's watchlist order.
- Imports, exports, and removes favorite movies and series through a private MDBList list named Silo Favorites, once a connection turns on **Sync favorites**.
- Imports and exports movie and series ratings, including removals.
- Sends live playback start, pause, and stop events.

MDBList has no favorites of its own. The plugin can keep Silo favorites in a private static list called **Silo Favorites**, which it creates in the MDBList account the first time it exports a favorite. Because that adds a list to the account, favorites sync is off until a connection turns it on.

## Setup

1. Install the plugin. It has no server-wide settings.
2. In Silo's watch-provider settings, connect each profile with that person's MDBList API key, from the API section of [MDBList preferences](https://mdblist.com/preferences/#api).
3. To sync favorites, turn on **Sync favorites** in the connection's settings, along with Silo's favorites import and export switches.

Silo stores the key encrypted with the profile's connection. The plugin sends it to MDBList as the `apikey` query parameter and keeps it out of every error message it returns.

## How sync works

- **Watch history import** reads the whole MDBList play history on every sync. MDBList's show and season rows only summarize episode state, so the plugin imports movie and episode plays and lets Silo work out season and series completion.
- **Watch history export** first reads MDBList's plays from a day before the earliest play in the batch. A play MDBList already holds at the same second is reported as unchanged and not written again. MDBList also folds a written play into a nearby existing one, so a retried export does not create a duplicate play. The read stops after two pages (2,000 plays) so a backlog of old plays does not spend the daily quota; when it stops early or fails, the plugin writes the plays and relies on that folding. An episode with its own TMDB or TVDB ID is sent by that ID. Any other episode is sent under its series by season and episode number, because MDBList matches its flat episode list only by those IDs. An episode MDBList does not know by its own ID is sent once more under its series. An episode with only an IMDb ID cannot be matched and is reported as not found.
- **Watchlist import** returns the full watchlist in MDBList's order on every sync, so Silo can mirror the order and treat missing titles as removed.
- **Favorites** live in the Silo Favorites list, found by name on every sync, while **Sync favorites** is on:
  - While it is off, the plugin makes no favorites requests and never creates the list. Silo still offers favorites to the plugin when its own favorites export switch is on, which Silo sets for every new connection, so each sync reports the favorites it could not send, with a note to turn on Sync favorites. Turn off favorites export on the connection to clear that warning without syncing favorites. Turning Sync favorites off later leaves the list in MDBList and removes nothing from Silo.
  - Import reads the whole list on every sync, so Silo can treat a missing title as unfavorited. An account without the list imports nothing and removes nothing; see [Favorites on more than one Silo server](#favorites-on-more-than-one-silo-server) for what happens when the list is replaced.
  - The first favorite export creates the list as a private list. When the account already has more than one list named Silo Favorites, every server uses the oldest. The plugin looks again right after it creates the list, and if another server created one at the same moment, it deletes its own new, empty list.
  - A favorite removal from an account without the list counts as done and costs no write.
  - Favorites sync for movies and series. A favorite episode or season is rejected.
- **Ratings import** reads every movie and show rating. Silo treats a rating missing from a complete read as removed, so the plugin claims a complete read only when it can trust one:
  - A read that MDBList pages by offset, or one without a shows list, is imported without removals, because offsets shift when ratings change during the read.
  - When a cursor-paged read repeats an entry or ends short of the total MDBList reported, ratings changed during the read. The plugin abandons that read, and the next sync reads every rating again.
  - A rated title with only MDBList's own ID could be any local title. Silo skips it and handles no removals of that kind in that sync.
- **Writes** go to MDBList in batches. For watch history, MDBList names each entry it could not find or process, so the plugin answers every play on its own: a title MDBList cannot find is reported as not found, a play it reports an error for is retried, and the rest of the batch counts as written. When MDBList refuses something without naming which entry, the plays it did not answer are retried. Watchlist, favorites, and ratings writes still retry the whole batch when any entry is not found. A removal that finds nothing to remove counts as done.

### Favorites on more than one Silo server

Connect each server's profile to the same MDBList account, with its own API key or a shared one, and turn on Sync favorites on each connection. Every server reads and writes the same Silo Favorites list, so a favorite added on one server reaches the others on their next sync. For a removal to reach the other servers too, turn on favorite removal sync for the connection on every server; Silo leaves it off by default. A server removes a favorite only after it has seen that title in the list, so a favorite it has not exported yet is never removed.

Keep the list's name, and don't delete the list. The plugin treats a renamed or deleted list as missing, and the next favorite export creates a new, empty Silo Favorites list. What happens next depends on removal sync:

- With removal sync off, each server's next sync sees that its earlier favorites are missing from the new list and exports them again.
- With removal sync on, each server removes those favorites from Silo instead, because a complete read of the new list no longer holds them. Silo has no way yet for a plugin to say a list was replaced.

### Rate limits

MDBList allows 1,000 requests a day on its free tier, plus a five-minute window limit. The plugin paces each API key to about one request a second. Each page of a read holds up to 1,000 MDBList entries, so a large history costs few requests. Favorites add one list lookup to each favorites read and to each sync that writes favorites; the first favorite export also creates the list and looks it up again. MDBList's five-minute limit covers every API key of an account, so each extra server connected to the same account draws on the same allowance. When MDBList answers with a rate limit, the plugin retries in place if MDBList asks for 15 seconds or less, at most twice. Otherwise it tells Silo to pause the connection for the time MDBList gave, or for an hour when MDBList gave none or kept limiting.

## Upgrading from the built-in provider

Connections carry over once you run a Silo release that maps this plugin to the built-in `mdblist` provider. Existing connections keep their API key, settings, watchlist and rating records, and export history, and need no new setup. The plugin reads MDBList in full on each sync, as the built-in provider did.

Silo's built-in provider never synced favorites, and upgrading doesn't turn them on: favorites wait until a connection turns on Sync favorites. Silo turns its own favorites switches on for every new connection, so a connection with favorites may show the warning described under How sync works until you choose one way or the other.

Some behavior differs slightly:

- Error messages no longer quote MDBList's response text, such as its validation details.
- A ratings read that MDBList changed during the read is retried on the next sync instead of being imported without removals. A ratings read without a shows list imports movie ratings without removals instead of treating them as complete.
- A play to export that has no IMDb, TMDB, or TVDB ID is reported as not found instead of failing five times first.

When a ratings read is imported without removals, the plugin says why, and Silo shows the reason with the sync run. This needs a Silo server built on silo-plugin-sdk v0.21 or later.

## Not yet supported

- Dropped shows. MDBList keeps a dropped-shows list, which neither the built-in provider nor this plugin syncs yet.

## Development

The plugin builds against `silo-plugin-sdk` v0.27.0. It declares a batch size of 1,000, which needs a Silo server built on silo-plugin-sdk v0.27.0 or later; an older server refuses its manifest.

```bash
make test
make build
./plugin manifest
```

`make build-all` produces static binaries for the platforms declared in `manifest.json`.

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md) before opening a pull request. Changes to authentication, reconciliation, idempotency, or the watch-sync contract should start as an issue.

## License

AGPL-3.0-only.
