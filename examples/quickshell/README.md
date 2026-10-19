# Quickshell example

`MediaSync.qml` is an **untested** example singleton showing how a
Quickshell bar can consume mediasync over its Unix socket: it subscribes,
applies the snapshot and events, extrapolates the position, and sends
commands. Quickshell is a consumer only. mediasync does not depend on it.

If the `Socket` type does not fit your Quickshell version, read the event
stream from the CLI instead:

```qml
Process {
    command: ["mediasyncctl", "--json", "watch"]
    running: true
    stdout: SplitParser {
        onRead: line => {
            const msg = JSON.parse(line);
            if (msg.type === "snapshot") { /* replace state with msg.snapshot */ }
            else if (msg.type === "disconnected") { /* service restarting */ }
            else { /* event: msg.type, msg.session_id, msg.session */ }
        }
    }
}
```

`mediasyncctl watch` reconnects by itself and emits a fresh `snapshot`
line after every reconnect. Send commands with
`mediasyncctl play_pause`-style invocations, e.g.
`Quickshell.execDetached(["mediasyncctl", "toggle"])`.
