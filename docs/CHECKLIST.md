# Implementation checklist

Status legend: `[x]` done and tested · `[~]` partially done · `[ ]` not started.

## Milestone 0 — Repository inspection and API verification
- [x] Inspect repository (empty: LICENSE + README stub)
- [x] Choose language / IPC / layout (docs/ARCHITECTURE.md D1–D2)
- [x] Verify Jellyfin API behaviour against a real Jellyfin **12.2.0** server
      (and 10.10.7 for comparison); findings in docs/JELLYFIN.md

## Milestone 1 — Core model + local MPRIS discovery
- [x] Versioned provider-independent session model (`internal/model`)
- [x] Provider interface and registry (`internal/provider`)
- [x] MPRIS provider: discovery, appearance/disappearance, metadata, capabilities
- [x] MPRIS provider: position (Seeked + bounded resync), controls, volume
- [x] Fake-D-Bus-player test harness on a private `dbus-daemon`
- [x] Tests: discovery, metadata, controls, restart, missing metadata, unsupported caps, timeouts, bus loss
- [x] Integration test with a real MPRIS player (mpv + mpv-mpris)

## Milestone 2 — Engine: selection, filtering, dedup, routing
- [x] Session store with stable IDs, ownership checks, change detection, events
- [x] Modes: local_only / remote_only / combined; filters
- [x] Deterministic, activity-based selection; explicit selection with grace period
- [x] Deduplication of a local player vs. its own Jellyfin session (evidence-based)
- [x] Command routing to the exact session, capability checks, timeouts
- [x] Post-command verification (verified / acknowledged)
- [x] Provider supervision with restart backoff

## Milestone 3 — Jellyfin provider
- [x] HTTP client (TLS verification, private CA, timeouts, header-only auth, redirect guard)
- [x] Login / token validation / logout with server-side revocation
- [x] Session polling, mapping, credential-free artwork URLs
- [x] Capability model (observable vs. controllable)
- [x] Commands (Playstate + SetVolume) with verification via immediate re-poll
- [x] Stale / disconnected sessions, reconnection with backoff, revoked tokens
- [x] Mock-server tests; integration test against real Jellyfin 12.2

## Milestone 4 — IPC API
- [x] JSON-RPC 2.0 over a Unix socket (0700 dir / 0600 socket / SO_PEERCRED check)
- [x] Methods: hello, sessions.*, player.command, subscribe, providers.status, settings.*, diagnostics.get, jellyfin.*, service.reload
- [x] Subscriptions with snapshot + revisioned events; bounded queues
- [x] Go client + `mediasyncctl` CLI (incl. reconnecting `watch`)
- [x] Tests: protocol errors, client disconnects, client limits, permissions, restart

## Milestone 5 — Configuration and security
- [x] Strict TOML config + validation + state overlay for IPC-updated settings
- [x] Secret Service credential store + explicit file fallback
- [x] No secrets in logs / errors / URLs / IPC (tested)
- [x] Tests: invalid config, credentials in config, credential-store errors

## Milestone 6 — Packaging and docs
- [x] systemd user unit, Makefile (build/test/lint/install/uninstall)
- [x] README, ARCHITECTURE, API, CONFIGURATION, PROVIDERS, JELLYFIN, TESTING, SECURITY, CHANGELOG, third-party notices
- [x] Example config, Quickshell example (untested), shell example
- [x] CI workflow (unit, race, real mpv, real Jellyfin 12.2 in Docker, golangci-lint)

## Known gaps / future work
- [ ] Test the SO_PEERCRED rejection path with a second user account (implemented, not automatically tested).
- [ ] Test against real desktop keyrings (GNOME Keyring, KWallet). The client is tested against a protocol-level fake.
- [ ] Run Feishin itself in an automated test (only the generic MPRIS path is tested, with a fake "Feishin" player and real mpv).
- [ ] Test real Jellyfin phone apps (Finamp, Jellyfin Android). The tests use a simulated client speaking the same protocol.
- [ ] Verify the Quickshell example on a real Quickshell installation.
- [ ] Optional: Jellyfin WebSocket push instead of polling.
- [ ] Optional: re-export the selected session as an MPRIS player for MPRIS-only widgets.
- [ ] Optional: more providers (e.g. KDE Connect, Chromecast, Plex).
