# Architecture

mediasync is a single user-level service (`mediasyncd`) plus a CLI client
(`mediasyncctl`). It collects media sessions from *providers*, presents them
through one provider-independent model, and routes playback commands back to
the exact provider and session a consumer chose.

```
                ┌────────────────────────── mediasyncd ───────────────────────────┐
 MPRIS players  │  ┌──────────────┐                                                │
 (session bus) ─┼─▶│ mpris        │─┐   ┌──────────────────────────────┐           │
                │  │ provider     │ │   │ engine                       │  ┌──────┐ │  JSON-RPC over
                │  └──────────────┘ ├──▶│  store · change detection    │─▶│ IPC  │◀┼─ Unix socket ── consumers
 Jellyfin       │  ┌──────────────┐ │   │  mode/filters · dedup        │  │server│ │  (0600, same UID)  (Quickshell,
 server(s) ─────┼─▶│ jellyfin     │─┘   │  selection · command routing │  └──────┘ │                     CLI, scripts)
 (HTTPS)        │  │ provider ×N  │◀────│  verification · events       │           │
                │  └──────────────┘     └──────────────────────────────┘           │
                │  config (TOML) · state (JSON) · credentials (Secret Service/file) │
                └─────────────────────────────────────────────────────────────────┘
```

## Packages

| Package | Responsibility |
|---------|----------------|
| `internal/model` | Versioned, provider-independent schema: `Session`, `Capability`, `Command`, structured `Error` codes, `ProviderStatus`. No provider-specific types. |
| `internal/provider` | The `Provider` and `Sink` interfaces, the optional `Refresher`, and a factory `Registry`. |
| `internal/provider/mpris` | Local players over the D-Bus session bus. |
| `internal/provider/jellyfin` | Jellyfin HTTP client and provider (one instance per server). |
| `internal/engine` | Aggregation, change detection, presentation settings (mode, filters), deduplication, selection, command routing and verification, event fan-out, provider supervision. |
| `internal/ipc` | Socket transport: listening, permissions, peer credentials, framing, limits, JSON-RPC envelopes. No business logic. |
| `internal/daemon` | Wires everything together and implements the API methods. |
| `internal/config` | TOML configuration (read-only), JSON state file (written by the service). |
| `internal/secrets` | Credential stores: Secret Service, permission-checked file, memory. |
| `internal/redact` | Removes credentials from URLs and strings; the `Secret` type that never prints. |
| `internal/client` | Go client for the API (used by `mediasyncctl` and tests). |
| `internal/testutil/*` | Private `dbus-daemon`, fake MPRIS player, fake Secret Service, mock Jellyfin server. |

## Data flow

1. A provider discovers sessions and calls `Sink.Upsert(session)` /
   `Sink.Remove(id)` / `Sink.SetStatus(status)`. It always sends complete
   session objects. The engine never merges partial updates.
2. The engine checks ownership: a provider can only modify sessions it
   created. A remote provider can therefore **never overwrite** a local
   player's data, even if the IDs collided.
3. Change detection drops updates that differ only in `last_seen` or in a
   position that agrees with extrapolation (±1.5 s). The fresher position
   sample is still stored, silently.
4. On every meaningful change the engine recomputes the view: deduplication
   annotations, `kind`, `hidden`, the selection. It diffs against the
   previous view and emits `session.added/updated/removed` and
   `selection.changed` events with a monotonically increasing revision.
5. The IPC layer translates engine events per subscription (filters,
   hidden sessions), so each client sees a consistent add/update/remove
   stream for *its* view.

## Decisions

### D1 — Go, two runtime dependencies
Go gives a single static binary, cheap concurrency for many small event
sources, and a standard library that covers HTTP, TLS, JSON and Unix
sockets. Dependencies: `godbus/dbus` (BSD-2-Clause, the de-facto Go D-Bus
library) and `BurntSushi/toml` (MIT). `golang.org/x/sys` (BSD-3-Clause)
comes with godbus. No web framework, no WebSocket library, no
Secret Service library: the Secret Service client is ~300 lines on top of
godbus. *Alternatives considered:* Python (runtime dependencies on every
desktop, slower startup), Rust (zbus/tokio/reqwest dependency stack much
larger for no functional gain here).

### D2 — IPC: JSON-RPC 2.0 over a Unix socket, not D-Bus
*Considered:* exporting a custom D-Bus interface, re-exporting an MPRIS
player, HTTP on localhost, a Unix socket.
- D-Bus is natural on Linux, but its type system makes evolving nested
  objects (sessions with optional fields) awkward. Signals give no snapshot
  semantics, and many consumers (shell scripts, Quickshell's `Socket`) find
  line-oriented JSON simpler.
- HTTP on localhost is reachable by every local user and by browsers (DNS
  rebinding, CSRF). That is a security liability for a control interface.
- A Unix socket in `$XDG_RUNTIME_DIR` with `0700`/`0600` permissions and an
  `SO_PEERCRED` UID check gives per-user isolation, works from any language,
  and supports push notifications on the same connection.

JSON-RPC 2.0 is a small, widely implemented standard. Newline-delimited
framing works with `socat`, `jq`, QML and plain sockets.
*Trade-off:* consumers that only speak MPRIS (e.g. existing bar widgets)
cannot use mediasync directly; an MPRIS re-export of the selected session
is a possible future addition (see CHECKLIST).

### D3 — Snapshot + revisioned events, no event replay
A client gets a complete, consistent snapshot atomically with its
subscription, then deltas. There is no event history: after a restart or
an overflow, clients re-subscribe and replace their state. This makes
reconnecting trivial and bounds memory: per-subscriber queues are fixed
size, and slow clients are dropped rather than slowing everyone down.

### D4 — Jellyfin: polling `/Sessions`, not the WebSocket
Jellyfin's WebSocket can push session lists, but the Go standard library
has no WebSocket client and the push stream still needs a polling fallback.
`GET /Sessions` every 2 s (configurable, ≥ 0.5 s) costs one small request,
and the server extrapolates `PositionTicks` between client reports. After
each command the provider polls immediately (rate-limited to one request
per 150 ms) to verify the effect. *Trade-off:* up to one poll interval of
latency for changes made outside mediasync. A WebSocket transport can be
added inside the provider without touching anything else.

### D5 — Capability model and honest success reporting
Capabilities are advertised only when the source says they are supported:
MPRIS `Can*` properties, introspected `Volume` writability; Jellyfin
`SupportsRemoteControl`, `PlayState.CanSeek`, `SupportedCommands`. The
engine checks capabilities before dispatch, and providers check again.

Verified against Jellyfin 12.2: the command endpoints return `204` even for
sessions that cannot receive commands. So a `204` is *not* treated as
success on its own. Commands are only sent to sessions with a live control
channel, and the result says whether the effect was observed (`verified`)
or only accepted (`acknowledged`). See [API.md](API.md#playercommand).

### D6 — Stable session IDs
- MPRIS: `mpris:<bus-name suffix>` (e.g. `mpris:Feishin`). A player
  restarting under the same name keeps its ID. If one connection owns
  several MPRIS names, it is shown once under the shortest name. Commands
  are addressed to the *unique* connection name observed at discovery, so
  they cannot reach a different process that took over the name.
- Jellyfin: `jellyfin:<server name>:<hash(server id, device id, client)>`.
  Jellyfin's own session ID is exposed as `provider_session_id` but not used
  as the key, because it changes when a client reconnects with a new version.

### D7 — Deduplication needs evidence beyond metadata
Feishin playing from Jellyfin appears twice: as an MPRIS player and as a
Jellyfin session. A remote session is folded into a local one only if:
- a configured `[[dedup.links]]` rule matches both, **and** the tracks agree
  (or the item ID is referenced); or
- the remote client is on this machine (its address is a local interface
  address or loopback, or its device name equals the hostname), **and**
  either the Jellyfin item ID appears in the local player's track
  ID/URL/artwork URL, or the tracks agree.

"Tracks agree" means equal normalised title, overlapping artists if both
are known, equal album if both are known, and durations within 3 s. Unknown
titles never match. Two phones playing the same song are **not** merged,
because nothing says they are the same device. The local session always wins
and keeps its own data. The remote one is hidden with `duplicate_of`, and
reachable with `include_hidden`. Address-based detection can be disabled
(`dedup.use_address = false`) for servers behind Docker's userland proxy,
where every client appears to come from a local address. This was observed
during testing: `RemoteEndPoint` was the Docker gateway.

### D8 — Selection follows activity
The automatic selection is the most recently *active* session: started
playing, paused, or controlled through mediasync. A preferred player that
is playing always wins. This was changed after manual testing showed that a
"playing beats paused" rule makes a play/pause key toggle a different
device after pausing the local player. Sessions already playing when a
provider (re)syncs carry no activity, so startup order cannot decide the
selection. The full order is in [API.md](API.md#selection).

### D9 — Configuration vs. state vs. credentials
- `config.toml` is written by the user and never modified by the service.
  Unknown keys are errors, and keys that look like credentials (`token`,
  `password`, `api_key`, …) are refused.
- `state.json` holds settings changed over IPC (which override the config)
  and a random device ID. It contains no secrets.
- Credentials live in the Secret Service, or in an explicitly enabled file
  backend. See [CONFIGURATION.md](CONFIGURATION.md#credentials).

### D10 — Supervision and failure handling
Each provider runs under the engine's supervisor. When `Run` returns, its
sessions are removed and it restarts with exponential backoff (1 s → 60 s).
The MPRIS provider exits when the bus connection is lost. The Jellyfin
provider handles outages itself: sessions become `disconnected` (not
controllable) right away and are removed after
`remove_disconnected_after`. Polling backs off up to 60 s, and the server
identity and token are re-validated on recovery. A rejected token (401)
moves the provider to `unauthenticated` without retry storms.

## Resource bounds

| Resource | Bound |
|----------|-------|
| IPC clients | `ipc.max_clients` (default 32) |
| IPC message size | 1 MiB |
| In-flight requests per connection | 16 |
| Outgoing queue per connection | 1024 messages (then disconnect) |
| Events queued per subscription | 256 (then `subscription.closed`) |
| D-Bus calls | `local.call_timeout` (3 s) each |
| MPRIS position reads | one per playing player per `local.position_resync` |
| Jellyfin requests | `request_timeout` (10 s); one poll per `poll_interval`; responses ≤ 8 MiB |
| Commands | `commands.timeout` (5 s) + verification window |
| systemd unit | `MemoryMax=128M`, `TasksMax=64` |

## Concurrency model

- The engine has one mutex. Provider callbacks, IPC queries and the
  supervisor all take it briefly, and no I/O happens under it. Event
  delivery to subscribers is non-blocking.
- Each provider has its own goroutine (`Run`). Commands are executed on the
  caller's goroutine with a timeout.
- Each IPC connection has a reader, a writer and up to 16 handler
  goroutines.
