# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/). The IPC API and session schema
are versioned separately (see docs/API.md).

## [Unreleased]

### Added
- Queue, lyrics and playlists (API version 1, additive): `sessions.queue`,
  `lyrics.get`, `library.playlists`, `player.command` `queue_jump` and
  `play_items`, the `queue.changed` event and the `queue`, `queue_jump`,
  `play_items` and `lyrics` capabilities. CLI: `queue`, `lyrics`,
  `playlists`, `jump`, `play-items`.
- Lyrics from Jellyfin (`/Audio/{id}/Lyrics`), from players that publish
  them over MPRIS (`xesam:asText`, e.g. Feishin), by Jellyfin item id for
  local players, and an optional LRCLIB fallback (`[lyrics] lrclib`, off by
  default).
- Local players borrow queue and lyrics from their linked Jellyfin session;
  queue commands are carried out through it (`via_session_id`).

### Changed
- View-only sessions keep read-only capabilities (`queue`, `lyrics`).
- Fake MPRIS test player uses its own property store (the godbus `prop`
  helper merges map values in place, keeping stale metadata and racing with
  replies).

### Known limitations
- Jellyfin 12.x does not expose remote clients' queues during playback
  (verified on 12.2.0), so `queue`/`queue_jump` are unavailable there.
- systemd `Type=notify`: `systemctl restart` waits until the socket is ready; `mediasyncctl` also waits briefly for a starting service.
- `jellyfin check` detects an app whose saved login belongs to another device ID (it then appears twice and is never controllable).
- `mediasyncctl jellyfin check` / `jellyfin.check`: diagnoses why a remote app
  is view-only. It tests WebSocket forwarding through the server URL and shows
  the server's view of each app. Troubleshooting guide in docs/JELLYFIN.md.

### Fixed
- Data race in the IPC server: a connection accepted while the server was closing could call `WaitGroup.Add` concurrently with `Wait` (found by the race detector in CI).
- `scripts/jellyfin-test-server.sh` retries setup calls while Jellyfin is still starting (it answers `/System/Info/Public` before other endpoints stop returning 503), which broke CI on a freshly pulled image.

## [0.1.0] — 2026-10-19

First release.

### Added
- `mediasyncd` user service and `mediasyncctl` CLI.
- Provider-independent session model (schema version 1) with explicit
  unknowns, capabilities, origin, connection status and session kinds.
- MPRIS provider: automatic discovery on the session bus, metadata,
  playback state, position (Seeked signal + bounded resync), capabilities
  from `Can*` properties and introspected volume writability; play, pause,
  play/pause, stop, next, previous, seek (absolute/relative), volume.
- Jellyfin provider (tested with Jellyfin 12.2): login/logout with token
  revocation, header-only authentication, session polling, credential-free
  artwork URLs, remote control gated on the client's control connection,
  stale-session detection, outage handling with backoff, multiple servers.
- Engine: local-only / remote-only / combined modes, filters,
  activity-based deterministic selection, explicit selection with restart
  grace period, evidence-based deduplication of local players and their
  Jellyfin sessions, exact command routing with verification
  (`verified` / `acknowledged`).
- JSON-RPC 2.0 API over a Unix socket: sessions, selection, commands,
  subscriptions with snapshots and revisioned events, provider status,
  runtime settings, diagnostics, Jellyfin login/logout, reload.
- Credential storage in the Secret Service, with an opt-in permission-checked
  file fallback.
- TOML configuration with strict validation; runtime overrides in a state
  file.
- systemd user unit, Makefile, CI workflow, documentation, examples
  (configuration, Quickshell, shell).
- Test suite with private D-Bus, fake MPRIS players, a fake Secret Service
  and a mock Jellyfin server; opt-in integration tests against real mpv and
  a real Jellyfin server.
