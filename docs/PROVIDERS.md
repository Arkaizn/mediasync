# Writing a provider

A provider connects one source of media sessions (a local protocol, a
media server, a cast ecosystem, …) to mediasync. The engine, IPC API,
selection, deduplication and command routing work unchanged for any
provider that follows this contract.

## The interface

```go
// internal/provider/provider.go
type Provider interface {
    Instance() string      // unique: "mpris", "jellyfin:home", "myprov:foo"
    Kind() string          // "myprov"
    Origin() model.Origin  // model.OriginLocal or model.OriginRemote
    Run(ctx context.Context, sink Sink) error
    Command(ctx context.Context, sessionID string, cmd model.Command) error
}

type Sink interface {
    Upsert(s model.Session)            // add or fully replace one of your sessions
    Remove(sessionID string)
    SetStatus(st model.ProviderStatus) // health for providers.status
}

// Optional: polling providers can refresh on demand (used to verify commands).
type Refresher interface{ Refresh() }
```

## Rules

1. **Own your IDs.** Prefix them with your kind (`myprov:…`). Keep them
   stable across metadata updates and, where you can, across reconnects of
   the same device or player. Do not derive them from track data. The engine
   ignores `Upsert`s for IDs owned by another provider.
2. **Send whole sessions.** `Upsert` replaces the stored session. Allocate
   fresh values and never mutate a session after handing it over.
3. **Unknown is `nil`.** Leave fields you do not know as `nil` (or the enum's
   unknown value). Never fill in placeholders like "Unknown Artist".
4. **Capabilities are promises.** Only advertise a capability when the
   source says it is supported. Set `Controllable` accordingly. The engine
   clears capabilities of non-controllable sessions.
5. **Position.** Set `PositionMS` together with `PositionAt` (when it was
   sampled) and `Rate` if known. Do not push updates just because time
   passed: the engine drops position changes that match extrapolation.
6. **Status.** Use `StatusStale` when the source stopped reporting but the
   session may still exist, and `StatusDisconnected` when you lost contact
   with the source. Report provider health with `SetStatus`
   (`starting`, `running`, `degraded`, `unauthenticated`, `error`).
7. **Commands.** `Command` must:
   - return only after the source accepted the command, or with a
     `*model.Error` (`session_not_found`, `unsupported_capability`,
     `session_not_controllable`, `timeout`, `provider_error`, …);
   - honour `ctx` (the engine applies `commands.timeout`);
   - re-check that the session supports the command. The engine already
     did, but your view may be fresher;
   - target exactly the session given, never a "default" player.
   The engine then verifies the effect by watching your `Upsert`s. If you
   poll, implement `Refresher` so verification is fast.
8. **Lifecycle.** `Run` blocks until `ctx` is cancelled (return `nil`) or a
   fatal error (return it; the engine removes your sessions and restarts you
   with backoff). Handle transient failures inside `Run` where you can, as
   the Jellyfin provider does.
9. **Secrets.** Use `internal/secrets` for credentials and
   `redact.Secret` for values in memory. Never put secrets in session
   fields, errors, status messages, logs or URLs. Pass exposed URLs through
   `redact.URL`.
10. **Bounded resources.** Every network or IPC call needs a timeout, and
    retries need backoff. Bound what you read.

## Registering

Providers are constructed by the daemon (`internal/daemon/daemon.go`,
`applyProviders`). To add one:

1. Create `internal/provider/myprov` with a `Config` type and `New`.
2. Add a configuration section in `internal/config` (validated, no
   secrets), and document it in `docs/CONFIGURATION.md`.
3. Start it in `applyProviders` under the matching enable flag. Local
   providers follow `local_enabled`, remote ones `remote_enabled`.
4. If it can identify itself as the same playback as a local player, add
   the identifying data to generic session fields (`player.address`,
   `player.device`, `track.track_id`) so the generic dedup rules
   (docs/ARCHITECTURE.md, D7) can use them. Do not add provider-specific
   code to the engine.

## Testing

- Unit-test against a fake of your source (see `testutil/fakejellyfin` for
  an HTTP example and `testutil/fakempris` for D-Bus), using
  `providertest.Sink` to observe output.
- Test missing data, unsupported commands, timeouts, disconnects and
  reconnects, and that secrets never appear in errors or status.
- Add an opt-in integration test under `test/integration` (build tag
  `integration`) that runs against the real thing when its environment
  variables are set, and skips with a clear message otherwise.
