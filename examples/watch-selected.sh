#!/bin/sh
# Print "Artist — Title [state] (player)" whenever the selected session or
# its playback changes. Requires jq. Shows how to consume the
# `mediasyncctl --json watch` stream from a script (it reconnects by itself
# and emits a fresh snapshot after each reconnect).
mediasyncctl --json watch | jq -n --unbuffered -r '
  foreach inputs as $m ({sel: null, out: null};
    if $m.type == "snapshot" then
      .sel = $m.snapshot.selected_id
      | .out = ([$m.snapshot.sessions[] | select(.id == $m.snapshot.selected_id)] | first)
    elif $m.type == "selection.changed" then
      .sel = (if $m.session_id == "" then null else $m.session_id end) | .out = $m.session
    elif $m.type == "session.updated" and $m.session_id == .sel then
      .out = $m.session
    else
      .out = null
    end;
    .out | select(. != null)
    | "\((.track.artists // ["?"]) | join(", ")) — \(.track.title // "?") [\(.playback.state)] (\(.player.name))")'
