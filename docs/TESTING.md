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
| Self-restart after the executable is replaced (not for missing/non-executable files) | `cmd/mediasyncd` `TestWatchReplacedDetectsNewExecutable`, `TestWatchReplacedIgnoresMissingAndNonExecutable` |
| Secret Service unavailable; explicit file fallback | `TestCredentialStoreUnavailable` |

| Queue (10.10 vs 12.2 behaviour), queue layout, lyrics (synced/unsynced/none/unknown, cached), playlists, play_items (playlist order, unknown ids, audio gating), queue_jump | `provider/jellyfin` `TestQueueOnJellyfin1010AndUnavailableOn122`, `TestQueueLayoutWebStyle`, `TestLyricsFromJellyfin`, `TestPlaylistsAndPlayItems`, `TestPlayItemsGatedOnPlayableAudio`, `TestQueueJump` |
| Play commands stay under the URL budget (249-track playlist, jumps in a 400-item queue), retries for stricter proxies (414 and 400), "too large" error mapping, window selection | `provider/jellyfin` `TestPlayItemsLargePlaylistStaysWithinURLBudget`, `TestQueueJumpOnLongQueueStaysWithinURLBudget`, `TestPlayCommandRetriesForStricterProxyAndMapsTooLong`, `TestPlayWindow` |
| New track after `queue_jump` reaches `session.updated`/`sessions.get`; a client that keeps reporting the old track (Finamp) gives `acknowledged` and no invented track; too-long error and its info log | `daemon` `TestEndToEndTrackAfterRemotePlayNow` |
| Shuffled `play_items`: permutation of the input (playlist plus expanded album, all modes), URL budget with a random cut, no start with the playing track, capability gating; `shuffle` only on `play_items`; linked sessions borrow `play_items_shuffle`; end to end over the socket | `provider/jellyfin` `TestPlayItemsShuffleIsAPermutation`, `TestPlayItemsShuffleRespectsURLBudget`, `TestShuffledPlayNowDoesNotStartWithTheCurrentTrack`, `TestShuffledPlayItemsNeedsTheCapability`; `model` `TestShuffleOnlyForPlayItems`, `TestPlayItemsRequiredCapability`; `engine` `TestPlayItemsShuffleCapabilityAndLinkedRouting`; `daemon` `TestEndToEndPlayItemsShuffle`; `mediasyncctl` `TestSplitPlayItemsArgs` |
| `set_shuffle`: MPRIS `Shuffle` write and gating (missing, read-only, no CanControl), Jellyfin `SetShuffleQueue` and gating on `SupportedCommands`, validation, end to end on a local player | `provider/mpris` `TestSetShuffle`, `TestShuffleCapabilityNeedsAWritableProperty`; `provider/jellyfin` `TestSetShuffleOnJellyfin`; `model` `TestSetShuffleRequiresTheShuffleCapability`; `daemon` `TestEndToEndSetShuffleOnLocalPlayer`; `mediasyncctl` `TestParseOnOff` |
| `queue_jump` strategies: N `NextTrack` for short forward jumps, `PlayNow` beyond the limit / backwards / to the current track / with steps disabled / without time budget; counting from the previous jump for a stale-reporting client, reset on a fresh report | `provider/jellyfin` `TestQueueJumpForwardUsesNextTrackAndFallsBack`, `TestQueueJumpStepsDisabledAndBudget`, `TestQueueJumpStepsWithStaleReportingClient` |
| Queue context: from a playlist (whole list, current index, jump with the whole list, end when playing elsewhere), album, shuffled, replaced by other `play_items`, checked against reported upcoming tracks, never guessed; position matching; `queue.changed` on start and end over the socket | `provider/jellyfin` `TestQueueContextFromPlaylist`, `TestQueueContextAlbumShuffleAndReplacement`, `TestQueueContextChecksReportedUpcomingTracks`, `TestContextPosition`; `daemon` `TestEndToEndQueueContext`; `mediasyncctl` `TestContextLine` |
| Queue context with Finamp's report sequence (old track + changed snapshot, new track + pre-switch snapshot, fresh snapshot), endings on real departures (pending and confirmed), time box and info log with the reason | `provider/jellyfin` `TestQueueContextSurvivesFinampSwitch`, `TestQueueContextEndsOnRealDeparture`, `TestQueueContextPendingTimeoutAndLogging` |
| Acceptance: `play_items` of a playlist on a simulated Finamp (old track + changed snapshot, new track + stale snapshot, stale reports, fresh snapshot) with service restarts while pending, in the middle of the stale reports and at the end; `sessions.queue` returns the context and the whole playlist with the reported track's index at every step | `daemon` `TestAcceptanceQueueContextWithFinampAndRestarts` |
| Saving and restoring contexts: saved only on change, restored as pending, waiting vs. confirmed rules after a restart, garbage ignored, contexts of absent sessions dropped | `provider/jellyfin` `TestQueueContextsSaveAndRestore`, `TestRestoredContextOfAbsentSessionIsDropped` |
| Removal grace: a session missing for a poll keeps its state and context and produces no events (also over the socket, with an explicit selection kept), commands during the blip, 404 → `session_unavailable`, removal once after `remove_missing_after`, `0s` = at once | `provider/jellyfin` `TestMissingSessionGraceKeepsSessionAndContext`, `TestCommandToMissingSessionGoneFromServer`, `TestMissingSessionRemovedAtOnceWithoutGrace`; `daemon` `TestEndToEndMissingSessionBlip` |
| Contexts checked by membership: shuffled context with the client's shuffle on survives NextTrack ×3 and repeats, a track outside ends it (reason logged); a non-shuffled context switches to membership when the client shuffles (reported list shown, jump in it, context kept) and back to the strict order check | `provider/jellyfin` `TestShuffledContextWithClientShuffleSurvivesNextTrack`, `TestContextSwitchesToMembershipWhenClientShuffles` |
| Expected track: set after an acknowledged `play_items`/`next` (chained in a context)/`queue_jump` on a stale-reporting client with the reported track unchanged, `current_expected` in the queue, cleared when the client reports the new id; `next` without a known order stays `null`; not shown before activation, cleared after the timeout and by the next command | `daemon` `TestEndToEndExpectedTrack`; `provider/jellyfin` `TestExpectedTrackTimeoutAndNextCommand` |
| MPRIS lyrics from `xesam:asText` | `provider/mpris` `TestLyricsFromMetadata` |
| LRC parsing, LRCLIB client | `lyrics` |
| Linked local session borrows queue/lyrics and routes queue commands; `queue.changed` without `session.updated`; view-only read caps; lyrics source order and fallback | `engine` `TestLinkedLocalBorrowsQueueLyricsAndRoutesCommands`, `TestQueueChangedEvents`, `TestViewOnlyKeepsReadOnlyCapabilities`, `TestLyricsSourcesInOrder` |
| New API end to end over the socket | `daemon` `TestEndToEndQueueLyricsPlaylists`, `TestQueueUnavailableOnJellyfin122` |

Not covered automatically: LRCLIB against the real service (blocked in the
development environment; tested against a local mock implementing its
documented API), rejection of connections from *another UID*
(the check is implemented with `SO_PEERCRED` but needs a second user
account to test), and real desktop keyrings (GNOME Keyring/KWallet). The
Secret Service client is tested against a protocol-level fake.

## Integration tests (opt-in)

```sh
# Real MPRIS player: needs mpv and the mpv-mpris plugin
#   (Debian/Ubuntu: apt install mpv mpv-mpris; Arch: pacman -S mpv mpv-mpris)
go test -tags integration -v -run TestRealMPRIS ./test/integration/

# Real Jellyfin: start a disposable server (Docker) and export its env vars
eval "$(scripts/jellyfin-test-server.sh start)"   # defaults to jellyfin/jellyfin:12.4
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
- `TestRealJellyfinQueueLyricsPlaylists` checks queue availability per
  server version (12.x: none; 10.10: full queue and `queue_jump`), synced
  and plain lyrics from `.lrc`/`.txt` files, playlists, `play_items` in
  playlist order, and refusal of unknown ids. Run it against both versions:
  `JELLYFIN_IMAGE=jellyfin/jellyfin:10.10.7 scripts/jellyfin-test-server.sh start`.
- `TestRealJellyfinPlayURLLimit` sends play commands straight to the
  server: 249 ids (8792-byte request target) get Kestrel's HTTP 414,
  mapped to "playlist too large for one command"; the 168 ids mediasync's
  budget allows (5957 bytes) are accepted and reach the client.
- `TestRealJellyfinPlayItemsShuffle` plays the test album and its artist
  with `"shuffle": true`: mediasync expands both itself, and the client
  receives a permutation of the three tracks (`verified`).
- `TestRealJellyfinSetShuffle` sends `set_shuffle` on and off to a client
  that declares `SetShuffleQueue`; the client receives the
  `GeneralCommand` with `ShuffleMode`, and its reported `PlaybackOrder`
  verifies both.
- `TestRealJellyfinQueueContextAndJumps` starts the "Road Trip" playlist:
  the session gets the playlist as its queue context (also on 12.x), and a
  jump resends the whole playlist with the start index. On 10.10, which
  exposes the reported queue, a forward jump is sent as `NextTrack`
  commands.
- `TestRealJellyfin` logs in through the daemon. It simulates a phone app
  that opens the real WebSocket control channel and applies commands, plus
  a view-only browser client. It checks classification, metadata,
  credential-free artwork URLs, verified pause/play/seek/volume/next
  delivered only to the phone, refusal for the view-only client, removal of
  remote control when the WebSocket closes, and token revocation on logout.

Each test skips with the reason when its prerequisites are missing; `go
test -v` shows `SKIP` lines.

## Results of the last full run

Play-command URL limit and remote track reporting fixes (Linux, Go 1.24.7,
dbus-daemon 1.14, mpv 0.37 + mpv-mpris 0.7.1):

- `go test -race ./...`: all packages pass; `golangci-lint`: 0 issues.
- `go test -tags integration ./test/integration/` against **Jellyfin
  12.4.0**, **12.2.0** and **10.10.7** (all provisioned by
  `scripts/jellyfin-test-server.sh`): `TestRealMPRISPlayerMPV`,
  `TestRealJellyfin`, `TestRealJellyfinQueueLyricsPlaylists` and
  `TestRealJellyfinPlayURLLimit` PASS on all three;
  `TestRealSessionBusDiscovery` SKIP (no desktop session).
- Finamp's handling of remote `Play` commands and its reporting while
  remote-controlled were read from its source (1.0.1, `playon_service.dart`,
  `playback_history_service.dart`) and reproduced with the fake server's
  `StaleTrackReports` client; a real iPhone was not part of the run.
- Not run: LRCLIB (host blocked in the environment), Feishin itself, real
  phone apps (simulated client using the same protocol, including the
  WebSocket `Play` message).
