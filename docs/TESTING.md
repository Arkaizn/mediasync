# Testing

```sh
make test-race          # unit + end-to-end tests (needs dbus-daemon)
make lint               # go vet, gofmt, golangci-lint (if installed)
make test-integration   # real mpv and/or real Jellyfin (opt-in, see below)
```

## What the automated tests cover

All D-Bus tests run against a **private `dbus-daemon`** started per test
(`internal/testutil/dbustest`). Fake players and the fake Secret Service are
real D-Bus services on that bus, so signals, name ownership, introspection
and method calls go through the actual daemon. Tests that need
`dbus-daemon` are skipped (and say so) when it is not installed.

| Area | Package / test |
|------|----------------|
| MPRIS discovery of existing and new players, identity, desktop entry | `provider/mpris` `TestDiscoveryOfExistingAndNewPlayers` |
| Metadata, playback status, capability and volume updates | `TestPlaybackAndMetadataUpdates` |
| Players disappearing, restarting (same stable ID), releasing names | `TestDisappearAndRestartKeepsStableID` |
| Missing and malformed metadata, players without control | `TestMissingMetadataAndCapabilities` |
| Read-only volume not advertised | `TestReadOnlyVolumeIsNotACapability` |
| All controls incl. SetPosition / relative Seek fallback | `TestControls`, `TestRelativeSeekAndSeekWithoutTrackID` |
| Unsupported commands never reach the player | `TestUnsupportedCommandsAreRejected` |
| D-Bus call timeouts | `TestCommandTimeout` |
| Include/ignore filters, one player owning several names | `TestIncludeIgnoreFilters`, `TestMultipleNamesOneConnectionAppearOnce` |
| Bounded position resync | `TestPositionResync` |
| Session-bus loss | `TestBusLossEndsRunAndRemovesSessions` |
| Jellyfin login, session discovery, field mapping, filtering (idle, other users, own device) | `provider/jellyfin` `TestUnauthenticatedThenLoginDiscoversSessions` |
| Token only in the Authorization header, never in URLs | `TestStoredTokenIsUsedAndNeverInURLs` |
| Commands reach exactly the intended client; multiple sessions | `TestCommandsReachTheExactClient` |
| View-only / unsupported commands not sent | `TestViewOnlyAndUnsupportedCommandsAreNotSent` |
| Session gone between poll and command | `TestSessionGoneBetweenPollAndCommand` |
| Stable IDs across Jellyfin session changes | `TestStableIDAcrossJellyfinSessionChange` |
| Stale sessions, position not invented | `TestStaleSessionWithoutControlChannel` |
| Server outage → disconnected → recovery; long outage removal; HTTP 500 | `TestServerOutageAndRecovery` |
| Revoked token → unauthenticated → re-login | `TestRevokedTokenBecomesUnauthenticated` |
| Logout revokes + deletes (also offline) | `TestLogoutRevokesAndDeletes` |
| Wrong password, credential store failure (token revoked again), unreachable server | `TestLoginFailures` |
| No secrets in errors/status; TLS verification and private CA; URL validation | `TestErrorsAndStatusNeverContainSecrets`, `TestTLSVerification`, `TestConfigValidation` |
| Modes local/remote/combined, filters, kinds, hidden sessions | `engine` `TestModesAndFilters` |
| Selection rules, explicit selection + grace period | `TestSelectionRules`, `TestPausingKeepsSelectionAndInitialSyncIsNotActivity`, `TestDeterministicOrdering` |
| Deduplication (same host, links, item IDs) and non-merging of unrelated sessions | `TestDedupSameHost`, `TestDedupContradictingMetadataNotMerged`, `TestDedupByLinkAndItemID` |
| Remote provider cannot overwrite local session | `TestProviderCannotOverwriteForeignSession` |
| Command routing, errors, outcomes (verified/acknowledged), provider errors, timeouts | `TestCommandRouting`, `TestCommandOutcomesAndFailures` |
| Events, extrapolation-aware change detection, slow subscribers | `TestEventsAndSnapshot`, `TestSlowSubscriberIsDropped` |
| Provider crash → sessions removed → restart | `TestProviderRestartRemovesSessionsAndRecovers` |
| Secret Service store/replace/delete, locked items + prompt, dismissed prompt, unavailable service, no default collection | `secrets` |
| File store permissions, corruption, ownership | `TestFileRejectsLoosePermissionsAndCorruption` |
| Config parsing, invalid configs, credentials in config refused, state file | `config` |
| **End to end** through the real socket: fake Feishin on D-Bus + mock Jellyfin + fake Secret Service; login, combined view with dedup, subscription events, routing to phone vs. local, mode switches, disabling providers at runtime, logout, no secrets in logs/diagnostics | `daemon` `TestEndToEndCombinedSessions` |
| Local-only without Jellyfin | `TestLocalOnlyWithoutJellyfin` |
| Player disappears while subscribed | `TestPlayerDisappearsWhileSubscribed` |
| IPC client disconnects release subscriptions; client limit | `TestIPCClientDisconnectsAndLimits` |
| JSON-RPC errors, oversized messages | `TestIPCProtocolErrors` |
| Socket permissions; second instance refused | `TestSocketSecurity` |
| Service restart; client re-snapshots; stale socket | `TestServiceRestartClientResnapshots` |
| Reload keeps running on invalid config | `TestReloadKeepsRunningOnInvalidConfig` |
| Secret Service unavailable; explicit file fallback | `TestCredentialStoreUnavailable` |

Not covered automatically: rejection of connections from *another UID*
(the check is implemented with `SO_PEERCRED` but needs a second user
account to test), and real desktop keyrings (GNOME Keyring/KWallet). The
Secret Service client is tested against a protocol-level fake.

## Integration tests (opt-in)

```sh
# Real MPRIS player: needs mpv and the mpv-mpris plugin
#   (Debian/Ubuntu: apt install mpv mpv-mpris; Arch: pacman -S mpv mpv-mpris)
go test -tags integration -v -run TestRealMPRIS ./test/integration/

# Real Jellyfin: start a disposable server (Docker) and export its env vars
eval "$(scripts/jellyfin-test-server.sh start)"   # defaults to jellyfin/jellyfin:12.2
go test -tags integration -v -run TestRealJellyfin ./test/integration/
scripts/jellyfin-test-server.sh stop

# Your own server (use a test account):
MEDIASYNC_IT_JELLYFIN_URL=https://jf.example MEDIASYNC_IT_JELLYFIN_USER=test \
MEDIASYNC_IT_JELLYFIN_PASSWORD=… go test -tags integration -v -run TestRealJellyfin ./test/integration/

# Read-only listing of the players on your desktop session bus:
MEDIASYNC_IT_SESSION_BUS=1 go test -tags integration -v -run TestRealSessionBus ./test/integration/
```

- `TestRealMPRISPlayerMPV` starts `mpv` (with `mpv-mpris`) on a private bus
  with two generated tones and drives it through the full daemon over IPC:
  discovery, metadata, capabilities, verified pause/play/seek
  (absolute and relative)/volume/next, and removal when mpv exits.
- `TestRealJellyfin` logs in through the daemon. It simulates a phone app
  that opens the real WebSocket control channel and applies commands, plus
  a view-only browser client. It checks classification, metadata,
  credential-free artwork URLs, verified pause/play/seek/volume/next
  delivered only to the phone, refusal for the view-only client, removal of
  remote control when the WebSocket closes, and token revocation on logout.

Each test skips with the reason when its prerequisites are missing; `go
test -v` shows `SKIP` lines.

## Results of the last full run

Recorded for the initial release (Linux, Go 1.24.7, dbus-daemon 1.14,
mpv 0.37 + mpv-mpris 0.7.1, Jellyfin 12.2.0 in Docker):

- `go test -race ./...`: all packages pass.
- `go test -tags integration ./test/integration/`: `TestRealMPRISPlayerMPV`
  PASS, `TestRealJellyfin` PASS (against a fresh container provisioned by
  `scripts/jellyfin-test-server.sh`), `TestRealSessionBusDiscovery` SKIP
  (no desktop session in the test environment).
- Feishin itself was **not** run: its MPRIS support is exercised through the
  generic MPRIS path (fake player named `Feishin`, and real `mpv`).
