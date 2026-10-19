# mediasync

**Universal Media Session Bridge**: one local service that lets desktop
shells, widgets and scripts see and control media playback on this Linux
session **and** on your other devices, through a single stable API.

- **Local players** via [MPRIS](https://specifications.freedesktop.org/mpris-spec/latest/)
  on the session D-Bus: Feishin, mpv, VLC, browsers, Spotify, …
  Discovered automatically from D-Bus, with no process sniffing.
- **Remote sessions** via **Jellyfin** (tested with Jellyfin 12.2): see what
  your phone is playing and control it, if the client allows remote control.
- **One session model** with explicit unknowns, capabilities, and
  local/remote/view-only/stale/disconnected states.
- **Modes**: `local_only`, `remote_only`, `combined`, plus filters by
  provider, origin, player, device and session.
- **Deduplication**: Feishin playing from Jellyfin shows up once, as the
  local player. Unrelated devices playing the same song are not merged.
- **Exact command routing** to the session you chose, with capability
  checks, timeouts, and honest results: `verified` or `acknowledged`.
- **Local JSON-RPC API** over a per-user Unix socket, with live events and
  full snapshots for reconnecting clients.
- **Secure by default**: tokens in your keyring (Secret Service), never in
  config files, logs, URLs or API responses; TLS verification always on;
  the socket is accessible only to your user; nothing listens on the network.

mediasync is independent of any shell. [Quickshell](https://quickshell.org)
is one possible consumer (see [`examples/quickshell`](examples/quickshell)).

```
$ mediasyncctl list
* mpris:Feishin                       playing  Feishin              Artist A — Track 1 [1:02/3:00] (local)
  jellyfin:home:1e54ee7457f628fd      paused   Pixel 7 (Finamp)     Someone — Song [0:13/4:10] (remote_controllable)
  jellyfin:home:99ed3dbe835f2758      playing  Firefox (Jellyfin Web) … (remote_view_only)
$ mediasyncctl pause --session jellyfin:home:1e54ee7457f628fd
pause → jellyfin:home:1e54ee7457f628fd: verified
```

## Requirements

- Linux with a D-Bus session bus (any desktop or window manager).
- Go ≥ 1.24 to build.
- Optional: a Secret Service provider (GNOME Keyring, KWallet, KeePassXC)
  for Jellyfin tokens; a Jellyfin server (12.x tested).

## Installation

```sh
git clone https://github.com/arkaizn/mediasync && cd mediasync
make                     # builds bin/mediasyncd and bin/mediasyncctl
make install             # → ~/.local/bin, ~/.config/systemd/user, example config
systemctl --user daemon-reload
systemctl --user enable --now mediasyncd
mediasyncctl status
```

`make install PREFIX=/usr/local` installs elsewhere. The unit file is
generated from `contrib/systemd/mediasyncd.service.in`. The example config is
copied to `~/.config/mediasync/config.toml` only if none exists yet.

### Running manually

```sh
mediasyncd                       # foreground, logs to stderr
mediasyncd --log-level debug
mediasyncd --config ./my.toml
mediasyncd --check-config        # validate and exit
```

## Quick start

Local players work out of the box: start a player and run `mediasyncctl list`.

To add Jellyfin, edit `~/.config/mediasync/config.toml`:

```toml
[[jellyfin.servers]]
name = "home"
url = "https://jellyfin.example.org"
```

then

```sh
mediasyncctl reload
mediasyncctl jellyfin login home      # prompts for user name and password
mediasyncctl list
```

The password is used once and never stored. The access token goes into your
keyring. Without a Secret Service, see
[the file fallback](docs/CONFIGURATION.md#credentials).

Useful commands:

```sh
mediasyncctl status                          # selected session + provider health
mediasyncctl toggle                          # play/pause the selected session
mediasyncctl next --session ID               # control a specific session
mediasyncctl seek +10s | seek 1:30 | volume 40
mediasyncctl select ID | select auto
mediasyncctl settings set mode=remote_only   # or local_only / combined
mediasyncctl settings set remote_enabled=false
mediasyncctl watch                           # live view (add --json for NDJSON)
mediasyncctl diag                            # diagnostics
```

## Using the API

Connect to `$XDG_RUNTIME_DIR/mediasync/mediasync.sock` and exchange JSON-RPC
2.0 messages, one per line. Call `subscribe` to get a snapshot plus live
events. The full reference is in **[docs/API.md](docs/API.md)**. Examples:
[`examples/quickshell/MediaSync.qml`](examples/quickshell/MediaSync.qml),
[`examples/watch-selected.sh`](examples/watch-selected.sh).

## Documentation

| | |
|-|-|
| [docs/API.md](docs/API.md) | IPC protocol, methods, events, errors, versioning |
| [docs/CONFIGURATION.md](docs/CONFIGURATION.md) | All settings, runtime settings, credentials |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | Design and the decisions behind it |
| [docs/JELLYFIN.md](docs/JELLYFIN.md) | Verified Jellyfin behaviour and client compatibility |
| [docs/PROVIDERS.md](docs/PROVIDERS.md) | Writing a new provider |
| [docs/TESTING.md](docs/TESTING.md) | Test suite, integration tests, last results |
| [SECURITY.md](SECURITY.md) | Security model and reporting |
| [CHANGELOG.md](CHANGELOG.md) | Release notes |

## Development

```sh
make build          # binaries in bin/
make test-race      # unit + end-to-end tests (needs dbus-daemon)
make lint           # go vet, gofmt, golangci-lint if installed
make test-integration   # real mpv / real Jellyfin, see docs/TESTING.md
```

## Removal

```sh
make uninstall                                  # stops the service, removes binaries and unit
mediasyncctl jellyfin logout NAME               # (before uninstalling) revoke + delete tokens
rm -rf ~/.config/mediasync ~/.local/state/mediasync   # configuration and state
```

If you used the Secret Service and did not log out, delete the items
labelled `mediasync: Jellyfin token …` in your keyring manager. You can
also revoke the device "mediasync" under *Dashboard → Devices* in Jellyfin.

## Status and limitations

- Providers: MPRIS (local) and Jellyfin (remote). The architecture accepts
  further providers ([docs/PROVIDERS.md](docs/PROVIDERS.md)).
- Jellyfin is polled (2 s by default) rather than streamed over WebSocket.
- Remote control depends on the Jellyfin client keeping a control
  connection. Command support differs between clients, which is why results
  say `verified` or `acknowledged`.
- Feishin was not run in the automated tests. Its MPRIS path is the same
  generic code tested with a fake Feishin player and with real `mpv`.
- The control API is local only. Network-wide control is out of scope.

## License

MIT, see [LICENSE](LICENSE). Third-party licenses:
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
