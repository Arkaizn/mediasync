# Jellyfin integration notes

The Jellyfin provider was built against, and tested with, **Jellyfin Server
12.2.0** (`jellyfin/jellyfin:12.2`, released 2026-10-05); the queue, play
command and URL-length behaviour below was re-checked on **12.4.0**. The behaviour
below was checked on a real server, not assumed from documentation. The
mock server in `internal/testutil/fakejellyfin` reproduces it, and
`test/integration` re-checks it against a real server.

## API calls used

| Purpose | Request | Notes |
|---------|---------|-------|
| Server identity | `GET /System/Info/Public` | Unauthenticated. `Id`, `ServerName`, `Version`. |
| Login | `POST /Users/AuthenticateByName` `{"Username","Pw"}` | Returns `AccessToken`, `User`, `ServerId`. Wrong password → 401. |
| Token check | `GET /Users/Me` | 401 if the token was revoked. |
| Sessions | `GET /Sessions` | All sessions visible to the user (admins see everyone; mediasync filters to the logged-in user unless `all_users`). |
| Playstate | `POST /Sessions/{id}/Playing/{Pause\|Unpause\|PlayPause\|Stop\|NextTrack\|PreviousTrack\|Seek}` | `Seek` takes `?seekPositionTicks=` (1 tick = 100 ns). Unknown command → 400; unknown session → 404; otherwise 204. |
| Volume | `POST /Sessions/{id}/Command` `{"Name":"SetVolume","Arguments":{"Volume":"0–100"}}` | 204. |
| Shuffle mode | `POST /Sessions/{id}/Command` `{"Name":"SetShuffleQueue","Arguments":{"ShuffleMode":"Shuffle"\|"Sorted"}}` | 204; delivered as `GeneralCommand` with the argument (verified on 12.4.0). Advertised (`shuffle`) only if `SupportedCommands` contains `SetShuffleQueue`; verified through `PlayState.PlaybackOrder` (`Shuffle`/`Default`). **Finamp declares `SetShuffleQueue` but its handler ignores it** (Finamp 1.0.1 source), so `set_shuffle` stays `acknowledged` there. |
| Logout | `POST /Sessions/Logout` | Revokes the calling token. |
| Artwork | `GET /Items/{id}/Images/Primary?tag=…` | Served **without authentication**, so artwork URLs carry no token. |

## Authentication

Every request sends
`Authorization: MediaBrowser Client="mediasync", Device="<hostname>", DeviceId="<random per install>", Version="…", Token="…"`.

**Verified on 12.2:** the legacy `X-Emby-Token` header and the `api_key`
query parameter are rejected with **401**. Only the `Authorization`
header works, including for the WebSocket. This also means tokens never
appear in URLs, which mediasync requires anyway.

The device ID is random, generated on first start, and stored in
`state.json`. mediasync's own session is excluded from the session list.

## Sessions and controllability

Fields used from each session: `Id`, `UserId`, `UserName`, `Client`,
`DeviceName`, `DeviceId`, `ApplicationVersion`, `RemoteEndPoint`,
`ServerId`, `SupportsRemoteControl`, `SupportedCommands`,
`LastPlaybackCheckIn`, `LastActivityDate`, `NowPlayingItem`
(`Id`, `Name`, `Album`, `AlbumId`, `AlbumArtist(s)`, `Artists`,
`RunTimeTicks`, `ImageTags`, `AlbumPrimaryImageTag`, `MediaType`) and
`PlayState` (`PositionTicks`, `CanSeek`, `IsPaused`, `IsMuted`,
`VolumeLevel`, `RepeatMode`, `PlaybackOrder`). Sessions without a
`NowPlayingItem` (idle clients) are not media sessions and are skipped.

Observed facts that shape the provider:

1. **`SupportsRemoteControl` is `true` only while the client holds a live
   WebSocket** to the server, even if it posted capabilities with
   `SupportsMediaControl: true`. Only then does the server deliver commands
   (as `Playstate`/`GeneralCommand` WebSocket messages).
2. **Command endpoints return 204 even when the target cannot receive the
   command** (no WebSocket). A 204 therefore proves nothing. mediasync
   sends commands only to sessions with `SupportsRemoteControl`, and treats
   a command as `verified` only once the client's next report shows the
   effect.
3. **When a client's WebSocket closes, the server removes its session at
   once.**
4. **A client without a WebSocket that stops reporting stays listed for
   about 10 minutes, and the server keeps extrapolating `PositionTicks`**
   (observed: the position kept advancing up to the track's end).
   mediasync marks such sessions `stale` when `LastPlaybackCheckIn` is older
   than `stale_after` (playing) or `stale_paused_after` (paused), and shows
   their position as unknown instead of the invented value. Clock skew
   between this machine and the server is corrected with the HTTP `Date`
   header.
5. `PositionTicks` is extrapolated by the server between client reports, so
   polling gives a usable position without client cooperation.
6. Playstate commands are not listed in `SupportedCommands`. Any client with
   remote control is expected to handle them. Not every client implements
   every one, which is why results are reported as `verified` or
   `acknowledged`. `seek` is advertised only if `PlayState.CanSeek`, and
   `volume` only if `SupportedCommands` contains `SetVolume`.
7. `RemoteEndPoint` is the address the server sees. Behind Docker's
   userland proxy this was the Docker gateway (`172.17.0.1`) for every
   client. See `dedup.use_address`.

## Queue, lyrics, playlists and play commands (verified on 12.2.0 and 10.10.7)

| Purpose | Request | Observed |
|---------|---------|----------|
| Queue | `NowPlayingQueue` in `GET /Sessions` | **12.x (12.2.0, 12.4.0): only the queue sent with the client's last *stop* report.** `SessionManager` stores it in `OnPlaybackStopped` only; start and progress reports do not change it. It is empty until the client's first track change, and then a snapshot from the last one (Finamp sends a stop report with its queue on every track change, so it is current right after one and frozen until the next). 10.10.7 stores it on start/progress and also fills `NowPlayingQueueFullItems` (absent on 12.x). 10.10 ignores empty queue reports. |
| Lyrics | `GET /Audio/{itemId}/Lyrics` | `{"Lyrics":[{"Text","Start"(ticks)?}]}`; `Start` missing for unsynced lyrics (`.txt`); 404 when there are none (also for unknown items); authentication required. `NowPlayingItem.HasLyrics` tells in advance. |
| Container tracks (shuffle) | `GET /Items?recursive=true&includeItemTypes=Audio&parentId=…` (album, folder) / `artistIds=…` / `genreIds=…`, `sortBy=AlbumArtist,Album,ParentIndexNumber,IndexNumber,SortName` | The tracks the server would play for the container; used to shuffle the whole list. Album and artist verified on 12.4.0. |
| Playlists | `GET /Items?recursive=true&includeItemTypes=Playlist&fields=ChildCount` | `GET /Playlists` is 405; entries via `GET /Playlists/{id}/Items` in playlist order. |
| Play | `POST /Sessions/{id}/Playing?playCommand=PlayNow\|PlayNext\|PlayLast&itemIds=…&startIndex=` | Delivered as WebSocket message `Play` `{ItemIds, PlayCommand, StartIndex}`. The server expands albums and playlists into tracks, but **reorders playlists**; unknown ids give an empty `ItemIds` with 204; clients without audio support or remote control also get 204. Invalid `playCommand` → 400. **The ids are accepted only in the query string**, and Kestrel rejects request lines over 8 KiB with **HTTP 414** (12.4.0: 230 ids = 8127 bytes pass, 240 ids = 8477 bytes fail; a 249-track playlist fails). Reverse proxies have similar limits (nginx `large_client_header_buffers 8k`, also 414) or smaller ones. |

Clients:
- **Finamp** reports only the *upcoming* tracks as `NowPlayingQueue` (its
  `peekQueue(next: 100)`), with `PlaylistItemId` set to its queue section
  names, and only with *Report queue to server* or *Play On* enabled.
  mediasync puts the current track in front. Finamp handles `PlayNow`
  (with `StartIndex`), `PlayNext` and `PlayLast`, and fetches the ids as
  audio items, so it relies on the server's album expansion.
- **Long playlists:** mediasync keeps the request target of a play command
  under 6000 bytes (about 170 tracks with Jellyfin's 32-character ids) and
  sends the part that fits: for `play_items` the beginning of the list, for
  `queue_jump` the target track and what follows it (then what precedes
  it), with a matching `startIndex`. It does not send the rest with
  follow-up `PlayLast` commands: Finamp handles `Play` messages
  concurrently and fetches `PlayNext`/`PlayLast` items sorted by
  `IndexNumber`, which would scramble the playlist order. The client also
  fetches the ids again (Finamp: `GET /Items?ids=…`, the same length
  limit), which is why the budget stays well below 8 KiB. If a stricter
  proxy still rejects the request (414, or 400 on a long URL), mediasync
  retries with half the items, up to three times, then fails with
  `provider_error` "playlist too large for one command" and the HTTP
  status in the error data.
- **Where a queue came from is only known when mediasync started it.**
  Jellyfin's session data has no field for a queue's source, and Finamp
  reports only the upcoming tracks (`peekQueue`) with section names as
  `PlaylistItemId`; its own `PlaylistItemId` for the session is the id of
  the queue's first item, not of the playlist. So mediasync records a
  context (playlist or album, the expanded track order, shuffled or not)
  only for its own `play_items` `now` of a single playlist or album, keeps
  it while the client plays from that list and its reported upcoming tracks
  follow the list's order, and drops it otherwise. Until the client reports
  a track from the list (Finamp: after its 90-second controlled mode, see
  below), the position in the context is unknown.
  Observed with Finamp 1.x on 12.4.0 right after the `Play` message: first
  a report with the **old track** and a **changed queue snapshot**, then the
  new track with the **pre-switch snapshot** (12.x keeps it until the next
  stop report), and only later a snapshot in playlist order. So the context
  stays pending while the old track is reported, whatever the snapshot
  does (time box: 10 minutes; Finamp reports the real track only with its
  next playback event after the 90 s); the upcoming order is only checked
  against snapshots reported after the first context track; and the
  context ends only on a real departure (a track outside it, a later
  snapshot out of order, or no context track within the time box). Each
  ending is logged at info level ("queue context ended", with the reason).
- **`queue_jump` without losing the client's queue:** a short forward jump
  is sent as `NextTrack` commands instead of `PlayNow` (which makes Finamp
  rebuild its queue from the sent tracks, losing the earlier tracks, its
  history and source). Finamp handles `NextTrack` itself (`skipToNext`), so
  its queue, shuffle state and source stay. On a context queue, `PlayNow`
  with the whole list keeps the list. Limits: the steps are counted in the
  reported queue (on 12.x a snapshot from the client's last track change),
  and a client that skips on its own between reports moves the count.
- **Finamp reports a remotely started track late (Finamp 1.0.1, from its
  source).** Any remote command (`Playstate` or `Play`) puts Finamp into a
  "controlled" mode for 90 seconds (`playOnStaleDelay`). In that mode its
  playback listener only sends progress reports, at most one per second,
  for the track it last recorded as current, and skips the step that
  records a new current track. After a remote `PlayNow` (`play_items`
  "now", `queue_jump`) or `NextTrack`, Finamp plays the new track but the
  server is told the previous track id, with the new track's position and
  state (often paused at 0:00 while it loads). The server only shows what
  the client reports, so `GET /Sessions` keeps the old `NowPlayingItem`
  until the 90 seconds have passed without a command and the next
  playback event (pause, seek, track change) makes Finamp report again.
  Finamp uses the same `DeviceId` for its WebSocket and its reports, so no
  second session appears (nothing is hidden or deduplicated). mediasync
  does not invent the track: such commands end as `acknowledged`, with a
  detail saying the player has not reported a new track. The fix belongs
  in Finamp.
- **Feishin** (Linux) publishes its lyrics in MPRIS `xesam:asText` as LRC
  and puts the Jellyfin item id into `mpris:trackid`
  (`…/track/<item id>`). It has no MPRIS TrackList (no queue over MPRIS).
  Its own remote-control server exposes its queue, which mediasync does not
  use yet.

## Session states in mediasync

| Jellyfin situation | mediasync |
|--------------------|-----------|
| Client with WebSocket playing/paused | `connected`, controllable, kind `remote_controllable` |
| Client without WebSocket, reporting | `connected`, view-only (`remote_view_only`) |
| Client without WebSocket, silent for > `stale_after` | `stale`, view-only, position `null` |
| Server unreachable | all its sessions `disconnected` (not controllable), removed after `remove_disconnected_after` |
| Token revoked (401) | provider `unauthenticated`, sessions removed |
| Client stops playback / disconnects | session removed |

## Client compatibility

Remote control needs a client that keeps a WebSocket connection and
implements the commands. Jellyfin Web, the official mobile apps, Finamp and
Feishin open WebSockets, but command support varies by client and version.
mediasync does not hard-code client capabilities: it reports what the
server says, and the command outcome shows what actually happened. Clients
were simulated in tests (`test/integration`: a client that authenticates,
posts capabilities, opens the WebSocket, applies commands and reports its
state, as real players do). Real phone apps were not part of the automated
tests.

## Troubleshooting: "my phone shows as view-only"

Run on the computer while music plays on the phone:

```sh
mediasyncctl jellyfin check            # or: mediasyncctl jellyfin check SERVER
```

It tests a WebSocket handshake through the configured server URL and lists
what the server knows about every app of your user:

```
WebSocket: FAIL — the WebSocket handshake failed (HTTP 404). A reverse proxy in front of Jellyfin
           probably does not forward WebSocket upgrades; remote control cannot work through this URL
Apps seen by the server (1):
  Finamp on iPhone — playing Track 1
    view-only: the app announced remote-control support but has no live control (WebSocket) connection …
```

**WebSocket FAIL** → the reverse proxy in front of Jellyfin drops WebSocket
upgrades. Apps still play music and report what they play (plain HTTP), but
the server can never push commands to them. This was reproduced with nginx
in front of Jellyfin 12.2: Finamp with *Play On* enabled stayed view-only.
Fix the proxy for the `/socket` path:

- **nginx:**
  ```nginx
  map $http_upgrade $connection_upgrade { default upgrade; '' close; }
  # inside the location that proxies Jellyfin:
  proxy_http_version 1.1;
  proxy_set_header Upgrade $http_upgrade;
  proxy_set_header Connection $connection_upgrade;
  ```
- **Nginx Proxy Manager:** enable *Websockets Support* on the proxy host.
- **Apache:** `RewriteEngine on`, `RewriteCond %{HTTP:Upgrade} websocket [NC]`,
  `RewriteRule /(.*) ws://JELLYFIN:8096/$1 [P,L]` (with `mod_proxy_wstunnel`).
- **Caddy, Traefik, Cloudflare Tunnel:** forward WebSockets by default. Check
  for extra middlewares or rules that block `/socket`.

**WebSocket OK but the app is still view-only:**
- The phone may use a different URL than mediasync, e.g. a public domain
  through the proxy vs. a LAN IP. The check only tests mediasync's URL.
- **The app appears twice** (same app and device name, one playing, one
  idle): its saved login token belongs to a different device ID than the
  app now uses (after a reinstall, update or backup restore). Verified on
  Jellyfin 12.2: the control WebSocket authenticates with the token and is
  attached to the token's device, so neither session is controllable. **Log
  out of the app and log in again.** `jellyfin check` points this out.
- The app's remote-control setting is off. In Finamp it is *Settings →
  Playback Reporting → Enable 'Play On' Support*. Finamp gives up
  reconnecting after a few minutes of failures. Close and reopen the app
  after fixing the proxy.
- The phone suspended the app: iOS while paused, or Android battery
  optimisation.

## Untested / out of scope

- Jellyfin versions older than 12 were not tested. 10.10.7 was probed
  during development and behaved the same for sessions and commands, but it
  still accepted `api_key`. mediasync never uses that either way.
- Quick Connect and API keys created in the dashboard: log in with a user
  account instead.
