# mediasync IPC API (version 1)

`mediasyncd` exposes a local API over a Unix domain socket. Any language
that can open a Unix socket and read/write JSON lines can use it (QML via
Quickshell's `Socket`, Python, shell with `socat`, Go, Rust, …).

- **Transport:** Unix stream socket at
  `$XDG_RUNTIME_DIR/mediasync/mediasync.sock` (override: `ipc.socket_path`).
  The directory is `0700`, the socket `0600`, and connections from other
  UIDs are rejected (`SO_PEERCRED`). Nothing listens on the network.
- **Framing:** one JSON object per line (`\n`-terminated, UTF-8). Maximum
  message size: 1 MiB (larger messages close the connection).
- **Protocol:** [JSON-RPC 2.0](https://www.jsonrpc.org/specification).
  Requests may be pipelined; responses can arrive out of order (match on
  `id`). Up to 16 requests per connection are processed concurrently.
- **Server notifications:** after `subscribe`, the server sends
  `{"jsonrpc":"2.0","method":"event","params":{…}}` lines.

```
$ echo '{"jsonrpc":"2.0","id":1,"method":"sessions.list"}' | socat - UNIX-CONNECT:$XDG_RUNTIME_DIR/mediasync/mediasync.sock
```

## Versioning

| Version          | Where                                  | Meaning |
|------------------|----------------------------------------|---------|
| `api_version`    | `hello`, `subscribe`, `diagnostics.get`| Major version of methods and envelopes. Breaking changes increment it. |
| `schema_version` | same                                   | Version of the session object schema. |

Within one major version, changes are additive only: new methods, new
optional fields, new event types, new enum values. **Clients must ignore
unknown fields and event types, and treat unknown enum values gracefully**
(for example, an unknown capability is simply not offered in the UI).

A client that needs a minimum version passes it to `hello`:
`{"api_version": 1}`. If the service is older, `hello` fails with
`invalid_argument` and `data.details.api_version` holds the version the service provides.

## Errors

Errors use the JSON-RPC error object. `error.data.code` carries a stable
mediasync error code; branch on that, not on the message text.

```json
{"jsonrpc":"2.0","id":7,"error":{"code":-32000,"message":"session \"x\" does not exist",
 "data":{"code":"session_not_found"}}}
```

| JSON-RPC `code` | Meaning |
|-----------------|---------|
| -32700 | Parse error (invalid JSON) |
| -32600 | Invalid request (not JSON-RPC 2.0) |
| -32601 | Method not found |
| -32602 | Invalid params (unknown fields, wrong types, invalid values); `data.code` = `invalid_argument` |
| -32603 | Internal error |
| -32000 | Service error; see `data.code` |

| `data.code` | Meaning |
|-------------|---------|
| `session_not_found` | No session with that ID (it may have just disappeared). |
| `no_session_selected` | A command without `session_id` while nothing is selected. |
| `unsupported_capability` | The session does not advertise the capability needed. `data.details.capabilities` lists what it supports. |
| `session_not_controllable` | The session can be observed but not controlled (e.g. a Jellyfin client without a remote-control connection). |
| `session_unavailable` | The session is disconnected (its server is unreachable). |
| `timeout` | The provider did not confirm the command in time. |
| `provider_error` | The player or server reported an error. |
| `provider_unavailable` | The provider cannot run (e.g. no session bus). |
| `provider_not_found` | Unknown Jellyfin server name. |
| `authentication_failed` | Wrong user name/password, or token rejected. |
| `not_authenticated` | No credentials stored for a server. |
| `invalid_argument` | Bad parameter value. |
| `invalid_config` | The configuration file is invalid (e.g. on `service.reload`). |
| `credential_store_error` | The credential backend failed (locked, dismissed prompt, bad permissions). |
| `credential_store_unavailable` | No Secret Service is running. |
| `server_unreachable` | Cannot connect to the Jellyfin server. |
| `tls_error` | TLS certificate verification failed. |
| `internal_error` | Bug; please report. |

No error, response or event ever contains passwords, tokens or URLs with
credentials.

## Session object (schema version 1)

Unknown information is `null`. Nothing is invented: a missing title stays `null`.
Pure position changes are **not** sent as events. Compute the current
position as `position_ms + (now - position_at) * (rate ?? 1)` while
`state == "playing"`.

```json
{
  "id": "jellyfin:home:6774b478785c485b",
  "provider": "jellyfin",
  "provider_instance": "jellyfin:home",
  "provider_session_id": "52e9bd39394127930fd68c1eb75a105a",
  "origin": "remote",
  "controllable": true,
  "capabilities": ["play","pause","play_pause","stop","next","previous","seek","volume"],
  "player": {
    "name": "Pixel 7 (Finamp)", "identity": "Finamp", "client": "Finamp", "client_version": "0.9",
    "device": "Pixel 7", "device_id": "…", "desktop_entry": null, "bus_name": null, "address": "192.168.1.50"
  },
  "server": {"instance": "home", "id": "e6a6…", "name": "jellyfin", "version": "12.2.0",
             "user_id": "1a62…", "user_name": "alice"},
  "track": {
    "title": "Track 1", "artists": ["Artist A"], "album": "Album X", "album_artists": ["Artist A"],
    "artwork_url": "https://jellyfin.example.org/Items/deec…/Images/Primary?tag=0b34…",
    "duration_ms": 180000, "track_id": "3b8d…", "url": null, "media_type": "Audio"
  },
  "playback": {
    "state": "playing", "position_ms": 10000, "position_at": "2026-10-19T09:20:33.100Z",
    "rate": null, "volume": 0.5, "muted": false, "shuffle": false, "repeat": "none"
  },
  "status": "connected",
  "last_seen": "2026-10-19T09:20:33.100Z",
  "updated_at": "2026-10-19T09:20:33.100Z",
  "error": null,
  "kind": "remote_controllable",
  "linked": ["…"],
  "duplicate_of": "…",
  "hidden": "duplicate"
}
```

| Field | Notes |
|-------|-------|
| `id` | Stable while the session exists; stays the same when a player restarts with the same bus name, or a Jellyfin client reconnects. Opaque. |
| `provider` / `provider_instance` | `mpris`, `jellyfin` / `mpris`, `jellyfin:<server name>`. |
| `origin` | `local` (this user session) or `remote` (another device, via a server). |
| `controllable` | `false` = view-only. |
| `capabilities` | Subset of `play`, `pause`, `play_pause`, `stop`, `next`, `previous`, `seek`, `volume`. Only advertised when the player supports them. |
| `playback.state` | `playing`, `paused`, `stopped`, `unknown`. |
| `playback.volume` | 0.0–1.0 (MPRIS players may report >1.0). |
| `playback.repeat` | `none`, `track`, `playlist`, or `null`. |
| `status` | `connected`, `stale` (no recent report; position is `null`), `disconnected` (server unreachable; last known data, not controllable). |
| `kind` | Summary: `local`, `remote_controllable`, `remote_view_only`, `stale`, `disconnected`. |
| `error` | Last error affecting the session: `{code, message, at}`. |
| `linked` | On a local session: IDs of remote sessions recognised as the same playback. |
| `duplicate_of` | On a hidden remote session: the local session that represents it. |
| `hidden` | Present when excluded from default listings: `duplicate`, `mode`, `view_only`, `stale`. |
| `track.artwork_url` | Never contains credentials. `file://` URLs from local players are passed through. |

## Selection

The service keeps one *selected* session; commands without `session_id`
go to it. Selection is either:

- `user`: set with `sessions.select`. It stays while the session exists. If
  the session disappears (e.g. a restarting player), it is restored when it
  comes back within 30 s; otherwise selection returns to automatic.
- `auto`: the first session in presentation order:
  1. the preferred player (`preferred_player`), if it is playing;
  2. sessions with observed activity before those without;
  3. most recent activity first. Activity means starting to play, pausing,
     or a command sent through mediasync. Pausing the current player keeps
     it selected, and a player that starts playing later takes over.
     Sessions already playing when a provider starts carry no activity;
  4. playing → paused → stopped/unknown;
  5. the preferred player;
  6. preferred origin (`prefer_origin`, default local);
  7. connected → stale → disconnected;
  8. controllable → view-only;
  9. session ID.

`sessions.list` returns sessions in this order.

## Methods

### `hello`
Params (all optional): `{"client": "my-widget", "api_version": 1}`
Result: `{"service":"mediasync","version":"0.1.0","api_version":1,"schema_version":1,"methods":[…]}`

### `sessions.list`
Params: a filter (all optional):
```json
{"providers": ["jellyfin"], "origins": ["local","remote"], "players": ["feishin"],
 "devices": ["Pixel 7"], "session_ids": ["…"], "include_hidden": false}
```
`providers` matches kind or instance. `players` uses the same matching as
`preferred_player`. `devices` matches the device name or ID. Hidden sessions
(by mode, settings or deduplication) are excluded unless `include_hidden`.

Result:
```json
{"revision": 42, "sessions": [ … ], "selected_id": "mpris:feishin",
 "selection_reason": "auto", "settings": { … }}
```

### `sessions.get`
Params: `{"session_id": "…"}` → `{"session": {…}}` (hidden sessions included).

### `sessions.selected`
→ `{"session": {…} | null, "reason": "user" | "auto" | ""}`

### `sessions.select`
Params: `{"session_id": "…"}` to select, or `{"session_id": null}` for automatic.
→ same as `sessions.selected`. Hidden sessions cannot be selected.

### `player.command`
Params:
```json
{"session_id": "…",              // optional; default: the selected session
 "command": "play" | "pause" | "play_pause" | "stop" | "next" | "previous" | "seek" | "set_volume",
 "position_ms": 60000,           // seek: absolute position, or
 "offset_ms": -10000,            // seek: relative offset (exactly one of the two)
 "volume": 0.5}                  // set_volume: 0.0–1.5
```

The command goes **only** to the provider that owns `session_id`. If that
session is gone, the call fails (`session_not_found`): it is never
redirected to another player.

Result:
```json
{"session_id": "…", "command": "pause", "outcome": "verified", "session": { … }}
```

**Success contract.** A result is only returned after the provider
*accepted* the command: the MPRIS method call returned without error, or the
Jellyfin server accepted the request for a session with an active
remote-control connection. The service then waits for the effect:

- `"outcome": "verified"`: the expected state change was observed (state
  became paused, track changed, position near the target, volume set, …).
  For Jellyfin this means the *client* reported the new state back.
- `"outcome": "acknowledged"`: the command was accepted but the effect was
  not observed within the verification window (`commands.verify_timeout`,
  default 1.5 s locally; `commands.remote_verify_timeout`, default 5 s
  remotely). `detail` explains. The command may still take effect, or have
  had no visible effect (e.g. `next` on the last track).

Failures are errors (see table above), e.g. `unsupported_capability` before
anything is sent, or `timeout` if the provider did not answer within
`commands.timeout`.

### `subscribe`
Params: a filter (as `sessions.list`). Result:
```json
{"snapshot": { …same as sessions.list… }, "api_version": 1, "schema_version": 1}
```
After the response, the connection receives `event` notifications for every
change after `snapshot.revision`. Calling `subscribe` again replaces the
previous subscription. `unsubscribe` stops events.

Event params:
```json
{"revision": 43, "type": "session.updated", "session_id": "…", "session": { … }}
```

| `type` | Fields | Meaning |
|--------|--------|---------|
| `session.added` | `session_id`, `session` | A session entered this subscription's view. |
| `session.updated` | `session_id`, `session` | Full new session object. |
| `session.removed` | `session_id`, `session` (last state) | Left the view (gone, filtered, hidden). |
| `selection.changed` | `session_id` (empty = none), `session`, `reason` | |
| `provider.status` | `provider` | A provider's health changed. |
| `provider.removed` | `provider_instance` | A provider was stopped (e.g. disabled). |
| `settings.changed` | `settings` | Presentation settings changed. |
| `subscription.closed` | `reason` (`overflow`, `shutdown`) | Events stopped. Re-subscribe. |

`revision` increases by one per service event. Because each subscription is
filtered, gaps in the revisions a client sees are normal. Events are only
lost if a client falls more than 256 events behind: the subscription is then
closed with `subscription.closed` (or the connection itself is closed if
more than 1024 messages are pending).

**Reconnecting.** A client never needs the event history. On connect, or
reconnect after a service restart or overflow, call `subscribe` again and
replace all local state with the returned snapshot. `mediasyncctl watch`
does this automatically.

### `providers.status`
→ `{"providers": [ProviderStatus…]}` where
```json
{"instance": "jellyfin:home", "kind": "jellyfin", "origin": "remote",
 "state": "running", "message": "", "last_error": null, "last_success": "…",
 "sessions": 2, "details": {"server_url": "…", "server_version": "12.2.0", "user_name": "alice", …}}
```
`state`: `starting`, `running`, `degraded` (server unreachable, retrying),
`unauthenticated` (log in), `error`, `disabled`, `stopped`.

### `settings.get`
→ `{"settings": {"mode": "combined", "preferred_player": "", "prefer_origin": "local",
"show_view_only": true, "show_stale": true, "dedup": true, "dedup_links": []},
"local_enabled": true, "remote_enabled": true, "overrides": {…}}`

### `settings.update`
Params: any subset of `mode`, `preferred_player`, `prefer_origin`,
`show_view_only`, `show_stale`, `dedup`, `local_enabled`, `remote_enabled`,
or `{"reset": true}` to drop all runtime overrides. Changes take effect
immediately (providers are started or stopped), are persisted in
`$XDG_STATE_HOME/mediasync/state.json`, and override the configuration
file. → same as `settings.get`.

### `diagnostics.get`
Version, uptime, paths, enabled providers, credential backend health,
engine state (selection, duplicates and their reasons), IPC statistics,
provider status and runtime memory. No secrets.

### `jellyfin.servers`
→ `{"servers": [{"name","url","enabled","instance","running","status"}]}`

### `jellyfin.login`
Params: `{"server": "home", "username": "alice", "password": "…"}`
(`server` may be omitted if exactly one is configured). The service
authenticates, stores **only the access token** in the credential store,
and starts polling. The password is not stored or logged.
→ `{"server_id","server_name","server_version","user_id","user_name","credential_backend"}`

### `jellyfin.logout`
Params: `{"server": "home"}`. Revokes the token on the server (best effort),
deletes it from the credential store and removes that server's sessions.
→ `{"revoked": true, "credential_deleted": true, "detail": "…"}`

### `service.reload`
Re-reads the configuration file. If it is invalid, the running
configuration is kept and `invalid_config` is returned. Socket and
credential-store changes need a restart. `SIGHUP` does the same.

## Example session (annotated)

```
→ {"jsonrpc":"2.0","id":1,"method":"hello","params":{"client":"bar","api_version":1}}
← {"jsonrpc":"2.0","id":1,"result":{"api_version":1,"schema_version":1,…}}
→ {"jsonrpc":"2.0","id":2,"method":"subscribe","params":{}}
← {"jsonrpc":"2.0","id":2,"result":{"snapshot":{"revision":12,"sessions":[…],"selected_id":"mpris:mpv",…}}}
← {"jsonrpc":"2.0","method":"event","params":{"revision":13,"type":"session.added","session_id":"jellyfin:home:1e54…",…}}
→ {"jsonrpc":"2.0","id":3,"method":"player.command","params":{"session_id":"jellyfin:home:1e54…","command":"pause"}}
← {"jsonrpc":"2.0","method":"event","params":{"revision":14,"type":"session.updated",…"state":"paused"…}}
← {"jsonrpc":"2.0","id":3,"result":{"session_id":"jellyfin:home:1e54…","command":"pause","outcome":"verified",…}}
```

## Command-line client

`mediasyncctl` wraps the API (`mediasyncctl --help`). `mediasyncctl --json
watch` prints the snapshot and then one event per line, and reconnects
on its own. Consumers that can only run processes can read that stream.
`mediasyncctl call METHOD '{json}'` sends raw requests.
