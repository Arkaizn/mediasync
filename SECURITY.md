# Security

## Model

- **Local only.** The API listens on a Unix socket in
  `$XDG_RUNTIME_DIR/mediasync/` (directory `0700`, socket `0600`). Every
  connection's peer UID is checked with `SO_PEERCRED` and must match the
  service's UID. No TCP port is opened.
- **Same-user trust.** Any process running as your user can use the API,
  just as it could talk to your MPRIS players or keyring directly. This
  includes `jellyfin.login`, which accepts a password.
- **Credentials.** Jellyfin passwords are only used for a single login
  request and are never stored or logged. Access tokens are stored in the
  Secret Service, or, only when explicitly configured, in a `0600` file
  that is **not encrypted at rest**. Tokens are sent only in the
  `Authorization` header over the configured server URL. They never appear
  in URLs, logs, errors, diagnostics or API responses. Configuration files
  containing credential-like keys are rejected.
- **TLS.** Certificate verification cannot be disabled. Private CAs can be
  added with `ca_file`. Redirects to other hosts are refused so tokens are
  not forwarded.
- **Bounded resources.** Message sizes, client counts, queues, timeouts and
  retries are bounded (see docs/ARCHITECTURE.md, *Resource bounds*). Slow
  clients are disconnected rather than allowed to grow memory.
- **Data minimisation.** By default only sessions of the logged-in Jellyfin
  user are shown, even for administrator accounts.

## Reporting a vulnerability

Please report security issues privately through GitHub's *Report a
vulnerability* (Security tab of the repository) rather than in public
issues. Include the version (`mediasyncctl version`) and steps to reproduce.
