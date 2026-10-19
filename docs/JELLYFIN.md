# Jellyfin integration notes

The Jellyfin provider was built against, and tested with, **Jellyfin Server
12.2.0** (`jellyfin/jellyfin:12.2`, released 2026-10-05). The behaviour
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

## Untested / out of scope

- Jellyfin versions older than 12 were not tested. 10.10.7 was probed
  during development and behaved the same for sessions and commands, but it
  still accepted `api_key`. mediasync never uses that either way.
- Quick Connect and API keys created in the dashboard: log in with a user
  account instead.
- Starting playback of a specific item ("play this album on my phone").
