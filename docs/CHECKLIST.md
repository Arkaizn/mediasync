# Implementation checklist

Status legend: `[x]` done and tested · `[~]` partially done · `[ ]` not started.

## Milestone 0 — Repository inspection and API verification
- [x] Inspect repository (empty: LICENSE + README stub)
- [x] Choose language / IPC / layout (see `docs/ARCHITECTURE.md`)
- [x] Verify Jellyfin API behaviour against a real Jellyfin **12.2.0** server
      (findings recorded in `docs/JELLYFIN.md`)

## Milestone 1 — Core model + local MPRIS discovery
- [ ] Versioned provider-independent session model (`internal/model`)
- [ ] Provider interface and registry (`internal/provider`)
- [ ] MPRIS provider: discovery, appearance/disappearance, metadata, capabilities
- [ ] MPRIS provider: playback position (Seeked + bounded resync), controls, volume
- [ ] Fake-D-Bus-player test harness on a private `dbus-daemon`
- [ ] Tests: discovery, metadata, controls, restart, missing metadata, unsupported caps

## Milestone 2 — Engine: selection, filtering, dedup, routing
- [ ] Session store with stable IDs and change events
- [ ] Modes: local_only / remote_only / combined; filters
- [ ] Deterministic selection and tie-breaking
- [ ] Deduplication of local player vs. the same playback reported by Jellyfin
- [ ] Command routing to the exact session, capability checks, timeouts
- [ ] Post-command verification (verified / acknowledged outcomes)
- [ ] Tests for all of the above

## Milestone 3 — Jellyfin provider
- [ ] HTTP client (TLS verification, timeouts, auth header, redaction)
- [ ] Login / token validation / logout
- [ ] Session polling, mapping, artwork URLs without credentials
- [ ] Capability model (observable vs. controllable)
- [ ] Commands (Playstate + SetVolume) with verification via re-poll
- [ ] Stale / disconnected sessions, reconnection with backoff
- [ ] Mocked-server tests; optional integration test against real server

## Milestone 4 — IPC API
- [ ] JSON-RPC 2.0 over a Unix socket (0700 dir / 0600 socket / SO_PEERCRED check)
- [ ] Methods: hello, sessions.*, player.command, providers.status, settings.*, diagnostics.get, jellyfin.*
- [ ] Subscriptions with snapshot + revisioned events; bounded client queues
- [ ] Go client library + `mediasyncctl` CLI
- [ ] Tests: protocol, client disconnects, slow clients, permissions

## Milestone 5 — Configuration and security
- [ ] TOML config + validation + state overlay for IPC-updated settings
- [ ] Secret Service credential store + explicit file fallback
- [ ] No secrets in logs / errors / URLs / IPC
- [ ] Tests: invalid config, credential-store errors

## Milestone 6 — Packaging and docs
- [ ] systemd user unit, Makefile (build/test/lint/install/uninstall)
- [ ] README, ARCHITECTURE, API, CONFIGURATION, PROVIDERS guide, CHANGELOG
- [ ] Example config, Quickshell example consumer
- [ ] CI workflow
