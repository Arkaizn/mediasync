# Configuration

| File | Purpose | Written by |
|------|---------|------------|
| `$XDG_CONFIG_HOME/mediasync/config.toml` (default `~/.config/mediasync/config.toml`) | Preferences and Jellyfin servers | you |
| `$XDG_STATE_HOME/mediasync/state.json` (default `~/.local/state/mediasync/state.json`) | Settings changed at runtime, device ID | the service |
| Secret Service, or `$XDG_STATE_HOME/mediasync/credentials.json` | Jellyfin access tokens | the service |
| `$XDG_RUNTIME_DIR/mediasync/mediasync.sock` | API socket | the service |

The service works without a configuration file. MPRIS discovery is on by
default and Jellyfin is only used when a server is configured. A fully
commented example is in [`examples/config.toml`](../examples/config.toml).

Check a configuration without starting the service:

```sh
mediasyncd --check-config [--config PATH]
```

Invalid files are rejected with the offending key or line. Unknown keys
are errors, so typos do not go unnoticed. Apply changes with
`mediasyncctl reload` (or `systemctl --user reload mediasyncd`). If the new
file is invalid, the service keeps running with the previous configuration.

## Reference

### `[general]`
| Key | Default | |
|-----|---------|-|
| `log_level` | `"info"` | `debug`, `info`, `warn`, `error` (CLI: `--log-level`) |
| `log_format` | `"text"` | `text` or `json` (logs go to stderr / the journal) |

### `[presentation]`
| Key | Default | |
|-----|---------|-|
| `mode` | `"combined"` | `combined`, `local_only`, `remote_only` |
| `preferred_player` | `""` | Player to prefer when it is playing. Matches the MPRIS bus name suffix (`feishin` → `org.mpris.MediaPlayer2.feishin*`), identity, desktop entry, or a remote client or device name; case-insensitive. |
| `prefer_origin` | `"local"` | Tie-breaker: `local`, `remote`, `none` |
| `show_view_only` | `true` | List remote sessions that cannot be controlled |
| `show_stale` | `true` | List stale and disconnected sessions |

### `[local]` — MPRIS
| Key | Default | |
|-----|---------|-|
| `enabled` | `true` | Discover players on the session bus |
| `include` | `[]` | If non-empty, only matching players |
| `ignore` | `["playerctld"]` | Players to skip (playerctld mirrors other players) |
| `position_resync` | `"5s"` | How often to re-read the position of *playing* players; `"0s"` disables (≥ 500 ms) |
| `call_timeout` | `"3s"` | Timeout for each D-Bus call |

### `[remote]`
| Key | Default | |
|-----|---------|-|
| `enabled` | `true` | Run the configured Jellyfin providers. `false` = local-only operation. |

### `[dedup]`
| Key | Default | |
|-----|---------|-|
| `enabled` | `true` | Fold a remote session into the local player that represents the same playback |
| `use_address` | `true` | Use the client address reported by Jellyfin for same-host detection. Set to `false` if Jellyfin runs behind Docker's userland proxy. |
| `links` | `[]` | Explicit rules, see below |

```toml
[[dedup.links]]
local_player = "feishin"     # matched like preferred_player
remote_client = "Feishin"    # and/or remote_device, remote_device_id
```
A link only merges sessions when they also play the same track.

### `[jellyfin]` (shared by all servers)
| Key | Default | |
|-----|---------|-|
| `poll_interval` | `"2s"` | 0.5 s – 5 min |
| `request_timeout` | `"10s"` | |
| `stale_after` | `"90s"` | A *playing* client without a remote-control connection that has not reported for this long becomes `stale` (its position is then shown as unknown). Jellyfin keeps such sessions for several minutes and keeps advancing their position. |
| `stale_paused_after` | `"15m"` | The same for paused clients, which often stop reporting |
| `remove_disconnected_after` | `"5m"` | While the server is unreachable, sessions stay visible as `disconnected` for this long |

### `[[jellyfin.servers]]`
| Key | Required | |
|-----|----------|-|
| `name` | yes | Short identifier (`[A-Za-z0-9_-]`, ≤ 32), used in session IDs and `mediasyncctl jellyfin login NAME` |
| `url` | yes | `http(s)://host[:port][/base]`. No credentials, query or fragment. |
| `ca_file` | no | Absolute path to an extra PEM CA bundle (private PKI). Verification is never disabled. |
| `all_users` | no | Include sessions of other users (requires an administrator account). Default: only your own. |
| `ignore_clients` | no | Client names to skip (e.g. `["Jellyfin Web"]`) |
| `ignore_devices` | no | Device names or IDs to skip |
| `enabled` | no | Default `true` |

### `[credentials]`
| Key | Default | |
|-----|---------|-|
| `store` | `"secret-service"` | `secret-service` or `file` |
| `file_path` | `$XDG_STATE_HOME/mediasync/credentials.json` | For `store = "file"`; absolute path |

### `[ipc]`
| Key | Default | |
|-----|---------|-|
| `socket_path` | `$XDG_RUNTIME_DIR/mediasync/mediasync.sock` | Absolute path. If `XDG_RUNTIME_DIR` is unset, `/tmp/mediasync-<uid>/`. |
| `max_clients` | `32` | 1–1024 |

### `[commands]`
| Key | Default | |
|-----|---------|-|
| `timeout` | `"5s"` | The provider must accept a command within this time |
| `verify_timeout` | `"1.5s"` | Wait to observe the effect on local players |
| `remote_verify_timeout` | `"5s"` | Wait to observe the effect on remote sessions |

## Runtime settings

These can be changed without editing the file:

```sh
mediasyncctl settings set mode=remote_only
mediasyncctl settings set preferred_player=feishin local_enabled=true
mediasyncctl settings set remote_enabled=false      # local-only operation
mediasyncctl settings reset                         # back to config.toml values
```

They are stored in `state.json` and take precedence over `config.toml`
until reset. Allowed keys: `mode`, `preferred_player`, `prefer_origin`,
`show_view_only`, `show_stale`, `dedup`, `local_enabled`, `remote_enabled`.

## Credentials

Log in once per server. The password is used for a single authentication
request and is never stored. Only the Jellyfin access token is kept.

```sh
mediasyncctl jellyfin login home                 # prompts for user and password
mediasyncctl jellyfin login home --user alice --password-stdin < pwfile
mediasyncctl jellyfin logout home                # revoke token + delete it
```

**Secret Service (default).** Tokens are stored in your keyring (GNOME
Keyring, KWallet ≥ 5.97 with its Secret Service, KeePassXC with Secret
Service integration, …) as an item labelled `mediasync: Jellyfin token for
NAME`, with attributes `application=mediasync`, `kind=jellyfin-token`,
`instance=NAME`, `server=URL`. If the keyring is locked, your desktop shows
its unlock prompt. If no Secret Service is running, login fails with
`credential_store_unavailable`, and local players keep working.

**File fallback** (`store = "file"`). For systems without a Secret
Service. Tokens are written to `credentials.json` with mode `0600` in a
`0700` directory. The service refuses to use the file if its permissions are
looser or another user owns it. **The token is not encrypted at rest**:
anyone who can read your files (or your backups) can use it until you log
out. Prefer the Secret Service when available. Logging out also revokes
the token on the server, so a leaked copy stops working.

**Disconnecting.** `mediasyncctl jellyfin logout NAME` asks the server to
revoke the token (best effort, it still works offline), deletes the stored
token, and removes that server's sessions. To stop using a server without
logging out, set `enabled = false` on it, or `remote_enabled=false`.
