# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/). The IPC API and session schema
are versioned separately (see docs/API.md).

## [Unreleased]

### Fixed
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
