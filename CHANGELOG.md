# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/). The IPC API and session schema
are versioned separately (see docs/API.md).

## [Unreleased]

### Added
- Session field `expected` (API v1, additive): while a player still reports
  the previous track after a remote command (Finamp for up to 90 s), the
  track mediasync sent is shown as `expected` (`track`, `since`, `reason`
  `play_items`/`queue_jump`/`next`/`previous`, `context_index`), clearly
  unconfirmed: `track`, `playback` and the position stay what the player
  reports. Set when the command ends `acknowledged` with the old track id;
  cleared when another track is reported, after 120 s, or with the next
  command. `next`/`previous` only where the order is unambiguous (a context
  in its own order). `sessions.queue` gets `current_expected` when
  `current_index` is that expected position.

### Fixed
- Queue contexts ended when the client's own shuffle reordered the queue
  (Finamp 1.0.2 with shuffle on, Jellyfin 12.4.0: "upcoming tracks do not
  follow the context order" after the first NextTrack). A context that was
  shuffled by mediasync, or while the client reports shuffle on, is now
  checked by membership (all reported tracks belong to it, any order,
  repeats allowed) and ends only when a track outside it is reported. In
  that mode `sessions.queue` shows the client's reported list with the
  context kept; a `queue_jump` there keeps the context too. Switching the
  client's shuffle on mid-context switches to this check instead of ending
  the context; the strict order check stays for unshuffled contexts with
  the client's shuffle off.

### Fixed
- A Jellyfin session missing from a single poll was removed at once:
  subscribers saw `session.removed`, `selection.changed` to auto and
  `session.added` with the same id within 1–2 s (Finamp during some
  `queue_jump`/`NextTrack` sequences), and its queue context and step-jump
  state were lost. Sessions are now removed only after they have been
  missing for the new `[jellyfin] remove_missing_after` (default `"6s"`;
  `"0s"` = the old behaviour). Until then the session stays as it was
  (selectable, explicit selection kept, context kept); commands are still
  sent, and a 404 from the server gives `session_unavailable`. Blips and
  removals are logged at info level.

### Added
- Queue contexts survive a service restart (mediasyncd restarts itself
  after every package update): they are kept in the state file
  (`provider_state`) and re-validated with the session's first report
  afterwards, like a pending context; contexts of sessions that are gone
  are dropped after the first poll.

### Changed
- `sessions.queue` `current_index` on a context follows the reported track
  whenever it is part of the context, also while the context still waits
  for confirmation (e.g. right after a restart); `null` while the reported
  track is outside it.

### Fixed
- Queue contexts ended about 1.5 s after `play_items` with Finamp (Jellyfin
  12.4.0), so `sessions.queue` never showed one. Right after the `Play`
  message Finamp reports the old track with a changed queue snapshot, then
  the new track with the pre-switch snapshot. A context now stays pending
  while the old track is reported (time box 10 minutes), checks the
  upcoming order only against snapshots reported after the switch, and
  ends only on a real departure. Endings are logged at info level with the
  reason.
- Consecutive `queue_jump` step jumps: the remembered position is kept
  while the reported track is unchanged (a changed snapshot alone no longer
  discards it); if the reported list changed in between, the jump is sent
  with `PlayNow` instead of counting steps from an unreliable position.

### Added
- Queue context: when mediasync starts a single playlist or album with
  `play_items` `now`, `sessions.queue` reports
  `"context": {"type", "id", "name", "shuffled"}` (new optional field,
  `null` otherwise; never guessed for queues started on the client), and,
  unless shuffled, the whole playlist in its order as `items`
  (`"source": "context"`, `current_index` = the reported track's position).
  The context ends when the client plays something else, reports a queue
  that does not follow it, gets another `play_items`, or disappears;
  starting and ending emit `queue.changed`. This also gives a queue (and
  `queue_jump`) on Jellyfin 12.x, which exposes none during playback.
  `mediasyncctl queue` prints "Playing from: NAME".
- `queue_jump` keeps the client's queue: short forward jumps
  (`jellyfin.queue_jump_max_steps`, default 15) are sent as that many
  `NextTrack` commands (`queue_jump_step_delay`, default 250 ms apart),
  counting from mediasync's previous jump while the client has not reported
  anything new; on a context queue the whole list is sent with `PlayNow` and
  the start index. Other jumps work as before.

### Changed
- An unconfirmed track change after a remote command whose position
  restarted under the old track id gets a detail saying so (still
  `acknowledged`).
- Command verification ignores queue changes mediasync makes itself (a
  context starting), using the player's own reported queue only.

### Added
- Shuffled playback: `player.command` `play_items` takes `"shuffle": true`
  (API version 1, additive) and needs the new capability
  `play_items_shuffle`, advertised wherever Jellyfin's `play_items` is and
  borrowed by linked local sessions. mediasync expands playlists, albums,
  artists, genres and folders into tracks, shuffles the whole list, and
  keeps the URL limit (shuffling before the cut). A shuffled "now" does
  not start with the track already playing. CLI:
  `mediasyncctl play-items ID... --shuffle`.
- `set_shuffle` command (`"shuffle": true|false`) and capability `shuffle`
  to switch a player's own shuffle mode: MPRIS players with a writable
  `Shuffle` property, Jellyfin clients that declare `SetShuffleQueue`
  (`GeneralCommand` with `ShuffleMode` `Shuffle`/`Sorted`, verified on
  12.4.0). Verified through `playback.shuffle`. CLI:
  `mediasyncctl shuffle on|off`.

### Fixed
- Jellyfin: `play_items` with a large playlist failed with `provider_error`.
  The ids travel in the request URL, and Jellyfin (Kestrel) answers HTTP 414
  above 8 KiB (verified on 12.4.0: a 249-track playlist). Play commands now
  stay under a 6000-byte budget: long lists are cut (the beginning for
  `play_items`; for `queue_jump` the target and what follows it), retried
  with fewer items if a stricter proxy still rejects them, and otherwise
  fail with "playlist too large for one command" and `http_status` in the
  error data. The 300-track limit is gone.
- `play_items` "now" was reported as `verified` when the session showed the
  previous track at 0:00. Finamp does this after remote commands: for 90 s
  it keeps reporting the previous track (a Finamp bug, see
  docs/JELLYFIN.md). Such commands are now `acknowledged`, with a detail
  saying the player has not reported a new track.
- "command failed" log lines include the error message and the HTTP status.

### Changed
- Docs: on Jellyfin 12.x the queue is the one sent with the client's last
  stop report (re-checked on 12.2.0 and 12.4.0), not "always empty".

## [0.2.0] — 2026-11-07

### Added
- AUR packages `mediasync` (releases, published automatically when a `v*`
  tag is pushed, so a plain `yay` updates it) and `mediasync-git`
  (`packaging/aur/`).
- The service restarts itself after its executable was replaced (package
  update, `make install`): it exits with status 75 and the systemd unit
  starts the new version (`RestartForceExitStatus=75`). Only when started
  by systemd.
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
