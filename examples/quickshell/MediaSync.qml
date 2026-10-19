// MediaSync.qml — example Quickshell singleton that mirrors mediasyncd state.
//
// UNTESTED EXAMPLE: written against the Quickshell.Io `Socket` and
// `SplitParser` types; adjust to your Quickshell version. Place it in your
// shell config (e.g. ~/.config/quickshell/services/MediaSync.qml) and use
// MediaSync.selected / MediaSync.sessions / MediaSync.command(...).
pragma Singleton
import QtQuick
import Quickshell
import Quickshell.Io

Singleton {
    id: root

    // All visible sessions (array of session objects, see docs/API.md).
    property var sessions: []
    property string selectedId: ""
    readonly property var selected: sessions.find(s => s.id === selectedId) ?? null
    property bool connected: socket.connected

    property int _nextId: 1
    property var _pending: ({})

    // Current position of a session in ms (extrapolated while playing).
    function position(s) {
        if (!s || s.playback.position_ms === null) return -1;
        let p = s.playback.position_ms;
        if (s.playback.state === "playing" && s.playback.position_at)
            p += (Date.now() - Date.parse(s.playback.position_at)) * (s.playback.rate ?? 1);
        return Math.max(0, p);
    }

    // command("play_pause"), command("seek", {position_ms: 60000}, id), …
    function command(name, args, sessionId) {
        const params = Object.assign({ command: name }, args ?? {});
        if (sessionId) params.session_id = sessionId;
        _call("player.command", params);
    }
    function select(sessionId) { _call("sessions.select", { session_id: sessionId ?? null }); }

    function _call(method, params, onResult) {
        const id = _nextId++;
        if (onResult) _pending[id] = onResult;
        socket.write(JSON.stringify({ jsonrpc: "2.0", id: id, method: method, params: params ?? {} }) + "\n");
        socket.flush();
    }

    function _applySnapshot(snap) {
        sessions = snap.sessions;
        selectedId = snap.selected_id ?? "";
    }

    function _applyEvent(ev) {
        switch (ev.type) {
        case "session.added":
        case "session.updated": {
            const list = sessions.filter(s => s.id !== ev.session_id);
            list.push(ev.session);
            sessions = list;   // order: re-sort if you need the service's ranking
            break;
        }
        case "session.removed":
            sessions = sessions.filter(s => s.id !== ev.session_id);
            break;
        case "selection.changed":
            selectedId = ev.session_id ?? "";
            break;
        case "subscription.closed":
            _subscribe();
            break;
        }
    }

    function _subscribe() {
        _call("subscribe", {}, result => _applySnapshot(result.snapshot));
    }

    Socket {
        id: socket
        path: (Quickshell.env("XDG_RUNTIME_DIR") ?? "/tmp") + "/mediasync/mediasync.sock"
        connected: true

        onConnectedChanged: {
            if (connected) root._subscribe();
            else reconnect.restart();
        }

        parser: SplitParser {
            onRead: line => {
                let msg;
                try { msg = JSON.parse(line); } catch (e) { return; }
                if (msg.method === "event") {
                    root._applyEvent(msg.params);
                } else if (msg.id !== undefined && root._pending[msg.id]) {
                    const cb = root._pending[msg.id];
                    delete root._pending[msg.id];
                    if (msg.result) cb(msg.result);
                } else if (msg.error) {
                    console.warn("mediasync:", msg.error.data?.code ?? msg.error.code, msg.error.message);
                }
            }
        }
    }

    // Reconnect after service restarts; the next subscribe returns a full snapshot.
    Timer {
        id: reconnect
        interval: 2000
        onTriggered: if (!socket.connected) { socket.connected = false; socket.connected = true; }
    }
}
