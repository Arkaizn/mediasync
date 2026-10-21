// Command mediasyncctl is a command-line client for mediasyncd.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/arkaizn/mediasync/internal/client"
	"github.com/arkaizn/mediasync/internal/config"
	"github.com/arkaizn/mediasync/internal/model"
)

var version = "0.1.0-dev"

const usage = `mediasyncctl — control the mediasync media session bridge

Usage:
  mediasyncctl [--socket PATH] [--json] COMMAND [ARGS]

Sessions:
  status                       selected session and provider health
  list [--all] [--provider P] [--origin local|remote] [--player P] [--device D]
  get SESSION_ID
  select SESSION_ID | auto
  watch                        stream a snapshot and live events (NDJSON with --json);
                               reconnects automatically

Playback (default target: the selected session):
  play | pause | toggle | stop | next | previous [--session ID]
  seek POSITION [--session ID] POSITION: 90, 1:30, 90s, +10s, -5s
  volume LEVEL [--session ID]  LEVEL: 0-100 (percent) or 0.0-1.0

Configuration:
  settings                     show settings
  settings set KEY=VALUE...    mode, preferred_player, prefer_origin, show_view_only,
                               show_stale, dedup, local_enabled, remote_enabled
  settings reset               drop runtime overrides
  providers                    provider status
  diag                         diagnostics
  reload                       re-read the configuration file

Jellyfin:
  jellyfin servers
  jellyfin login SERVER [--user NAME] [--password-stdin]
  jellyfin logout SERVER
  jellyfin check [SERVER]      diagnose remote control (WebSocket path, per-app status)

Other:
  call METHOD [JSON_PARAMS]    raw API call
  version
`

type globals struct {
	socket string
	json   bool
}

func main() {
	g := globals{socket: socketPath()}
	args := os.Args[1:]
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		switch {
		case args[0] == "--json":
			g.json = true
			args = args[1:]
		case args[0] == "--socket" && len(args) > 1:
			g.socket = args[1]
			args = args[2:]
		case strings.HasPrefix(args[0], "--socket="):
			g.socket = strings.TrimPrefix(args[0], "--socket=")
			args = args[1:]
		case args[0] == "-h" || args[0] == "--help":
			fmt.Print(usage)
			return
		default:
			fail(fmt.Errorf("unknown option %s", args[0]))
		}
	}
	if len(args) == 0 {
		fmt.Print(usage)
		os.Exit(2)
	}
	if err := runCommand(g, args[0], args[1:]); err != nil {
		fail(err)
	}
}

func socketPath() string {
	if v := os.Getenv("MEDIASYNC_SOCKET"); v != "" {
		return v
	}
	p := config.DefaultPaths()
	if cfg, err := config.Load(p.ConfigFile); err == nil {
		p = cfg.ResolvePaths(p)
	}
	return p.Socket
}

func fail(err error) {
	var ce *client.Error
	if errors.As(err, &ce) && ce.RPCCode == -32601 {
		fmt.Fprintf(os.Stderr, "mediasyncctl: %s\n", ce.Message)
		fmt.Fprintln(os.Stderr, "The running mediasyncd is older than this mediasyncctl. Restart it after updating:")
		fmt.Fprintln(os.Stderr, "  systemctl --user restart mediasyncd")
		os.Exit(1)
	}
	if errors.As(err, &ce) {
		fmt.Fprintf(os.Stderr, "mediasyncctl: %s (%s)\n", ce.Message, ce.Code)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "mediasyncctl:", err)
	os.Exit(1)
}

func dial(g globals) *client.Client {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := client.Dial(ctx, g.socket)
	if err != nil {
		fail(fmt.Errorf("cannot connect to mediasyncd at %s: %w (is the service running?)", g.socket, err))
	}
	return c
}

func call(g globals, method string, params any, out any) error {
	c := dial(g)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return c.Call(ctx, method, params, out)
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func runCommand(g globals, cmd string, args []string) error {
	switch cmd {
	case "version":
		fmt.Println("mediasyncctl", version)
		var hello map[string]any
		if err := call(g, "hello", map[string]any{"client": "mediasyncctl"}, &hello); err == nil {
			fmt.Printf("mediasyncd %v (api %v, schema %v)\n", hello["version"], hello["api_version"], hello["schema_version"])
		}
		return nil
	case "status":
		return cmdStatus(g)
	case "list", "ls":
		return cmdList(g, args)
	case "get":
		if len(args) != 1 {
			return errors.New("usage: get SESSION_ID")
		}
		var res struct {
			Session model.Session `json:"session"`
		}
		if err := call(g, "sessions.get", map[string]any{"session_id": args[0]}, &res); err != nil {
			return err
		}
		printJSON(res.Session)
		return nil
	case "select":
		if len(args) != 1 {
			return errors.New("usage: select SESSION_ID | auto")
		}
		var id any = args[0]
		if args[0] == "auto" {
			id = nil
		}
		var res struct {
			Session *model.Session `json:"session"`
			Reason  string         `json:"reason"`
		}
		if err := call(g, "sessions.select", map[string]any{"session_id": id}, &res); err != nil {
			return err
		}
		if g.json {
			printJSON(res)
		} else if res.Session != nil {
			fmt.Printf("selected %s (%s)\n", res.Session.ID, res.Reason)
		} else {
			fmt.Println("no session selected")
		}
		return nil
	case "play", "pause", "toggle", "stop", "next", "previous", "prev":
		name := map[string]model.CommandName{"play": model.CmdPlay, "pause": model.CmdPause, "toggle": model.CmdPlayPause,
			"stop": model.CmdStop, "next": model.CmdNext, "previous": model.CmdPrevious, "prev": model.CmdPrevious}[cmd]
		fs := flag.NewFlagSet(cmd, flag.ExitOnError)
		session := fs.String("session", "", "target session id")
		_ = fs.Parse(args)
		return sendCommand(g, map[string]any{"session_id": *session, "command": name})
	case "seek":
		fs := flag.NewFlagSet(cmd, flag.ExitOnError)
		session := fs.String("session", "", "target session id")
		pos, rest := splitPositional(args)
		_ = fs.Parse(rest)
		if pos == "" {
			return errors.New("usage: seek POSITION [--session ID]")
		}
		p := map[string]any{"session_id": *session, "command": model.CmdSeek}
		ms, relative, err := parsePosition(pos)
		if err != nil {
			return err
		}
		if relative {
			p["offset_ms"] = ms
		} else {
			p["position_ms"] = ms
		}
		return sendCommand(g, p)
	case "volume", "vol":
		fs := flag.NewFlagSet(cmd, flag.ExitOnError)
		session := fs.String("session", "", "target session id")
		lvl, rest := splitPositional(args)
		_ = fs.Parse(rest)
		v, err := parseVolume(lvl)
		if err != nil {
			return err
		}
		return sendCommand(g, map[string]any{"session_id": *session, "command": model.CmdSetVolume, "volume": v})
	case "watch":
		return cmdWatch(g)
	case "providers":
		var res struct {
			Providers []model.ProviderStatus `json:"providers"`
		}
		if err := call(g, "providers.status", nil, &res); err != nil {
			return err
		}
		if g.json {
			printJSON(res)
			return nil
		}
		for _, p := range res.Providers {
			printProvider(p)
		}
		return nil
	case "settings":
		return cmdSettings(g, args)
	case "diag", "diagnostics":
		var res map[string]any
		if err := call(g, "diagnostics.get", nil, &res); err != nil {
			return err
		}
		printJSON(res)
		return nil
	case "reload":
		if err := call(g, "service.reload", nil, nil); err != nil {
			return err
		}
		fmt.Println("configuration reloaded")
		return nil
	case "jellyfin", "jf":
		return cmdJellyfin(g, args)
	case "call":
		if len(args) < 1 {
			return errors.New("usage: call METHOD [JSON_PARAMS]")
		}
		var params any
		if len(args) > 1 {
			if err := json.Unmarshal([]byte(args[1]), &params); err != nil {
				return fmt.Errorf("invalid JSON params: %w", err)
			}
		}
		var res any
		if err := call(g, args[0], params, &res); err != nil {
			return err
		}
		printJSON(res)
		return nil
	case "help":
		fmt.Print(usage)
		return nil
	}
	return fmt.Errorf("unknown command %q (see --help)", cmd)
}

// splitPositional returns the first non-flag argument and the rest.
func splitPositional(args []string) (string, []string) {
	var pos string
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "--") {
			rest = append(rest, a)
			if !strings.Contains(a, "=") && i+1 < len(args) {
				rest = append(rest, args[i+1])
				i++
			}
			continue
		}
		if pos == "" {
			pos = a
		} else {
			rest = append(rest, a)
		}
	}
	return pos, rest
}

func parsePosition(s string) (ms int64, relative bool, err error) {
	sign := int64(1)
	if strings.HasPrefix(s, "+") || strings.HasPrefix(s, "-") {
		relative = true
		if s[0] == '-' {
			sign = -1
		}
		s = s[1:]
	}
	if strings.Contains(s, ":") {
		parts := strings.Split(s, ":")
		var total int64
		for _, p := range parts {
			n, err := strconv.ParseInt(p, 10, 64)
			if err != nil || n < 0 {
				return 0, false, fmt.Errorf("invalid position %q", s)
			}
			total = total*60 + n
		}
		return sign * total * 1000, relative, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return sign * d.Milliseconds(), relative, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, false, fmt.Errorf("invalid position %q", s)
	}
	return sign * int64(f*1000), relative, nil
}

func parseVolume(s string) (float64, error) {
	s = strings.TrimSuffix(s, "%")
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid volume %q", s)
	}
	if f > 1 || strings.Contains(s, "%") {
		f /= 100
	}
	return f, nil
}

func sendCommand(g globals, p map[string]any) error {
	if p["session_id"] == "" {
		delete(p, "session_id")
	}
	var res model.CommandResult
	if err := call(g, "player.command", p, &res); err != nil {
		return err
	}
	if g.json {
		printJSON(res)
		return nil
	}
	fmt.Printf("%s → %s: %s", res.Command, res.SessionID, res.Outcome)
	if res.Detail != "" {
		fmt.Printf(" (%s)", res.Detail)
	}
	fmt.Println()
	return nil
}

func str(p *string, fallback string) string {
	if p == nil || *p == "" {
		return fallback
	}
	return *p
}

func fmtMS(ms *int64) string {
	if ms == nil {
		return "?"
	}
	s := *ms / 1000
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

func describe(s model.Session, selected bool) string {
	mark := " "
	if selected {
		mark = "*"
	}
	artist := "?"
	if len(s.Track.Artists) > 0 {
		artist = strings.Join(s.Track.Artists, ", ")
	}
	pos := s.Playback.CurrentPosition(time.Now())
	line := fmt.Sprintf("%s %-40s %-8s %-20s %s — %s [%s/%s] (%s", mark, s.ID, s.Playback.State, s.Player.Name,
		artist, str(s.Track.Title, "?"), fmtMS(pos), fmtMS(s.Track.DurationMS), s.Kind)
	if s.Hidden != "" {
		line += ", hidden: " + s.Hidden
	}
	if s.DuplicateOf != nil {
		line += ", duplicate of " + *s.DuplicateOf
	}
	return line + ")"
}

type snapshot struct {
	Revision   uint64          `json:"revision"`
	Sessions   []model.Session `json:"sessions"`
	SelectedID *string         `json:"selected_id"`
	Reason     string          `json:"selection_reason"`
}

func cmdList(g globals, args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	all := fs.Bool("all", false, "include hidden sessions")
	prov := fs.String("provider", "", "provider kind or instance")
	origin := fs.String("origin", "", "local or remote")
	player := fs.String("player", "", "player name")
	device := fs.String("device", "", "device name or id")
	_ = fs.Parse(args)
	f := map[string]any{"include_hidden": *all}
	if *prov != "" {
		f["providers"] = []string{*prov}
	}
	if *origin != "" {
		f["origins"] = []string{*origin}
	}
	if *player != "" {
		f["players"] = []string{*player}
	}
	if *device != "" {
		f["devices"] = []string{*device}
	}
	var snap snapshot
	if err := call(g, "sessions.list", f, &snap); err != nil {
		return err
	}
	if g.json {
		printJSON(snap)
		return nil
	}
	if len(snap.Sessions) == 0 {
		fmt.Println("no sessions")
	}
	for _, s := range snap.Sessions {
		fmt.Println(describe(s, snap.SelectedID != nil && *snap.SelectedID == s.ID))
	}
	return nil
}

func printProvider(p model.ProviderStatus) {
	line := fmt.Sprintf("%-20s %-16s sessions=%d", p.Instance, p.State, p.Sessions)
	if p.Message != "" {
		line += "  " + p.Message
	}
	if p.LastError != nil && p.State != model.ProviderRunning {
		line += fmt.Sprintf("  [%s: %s]", p.LastError.Code, p.LastError.Message)
	}
	fmt.Println(line)
}

func cmdStatus(g globals) error {
	c := dial(g)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var sel struct {
		Session *model.Session `json:"session"`
		Reason  string         `json:"reason"`
	}
	if err := c.Call(ctx, "sessions.selected", nil, &sel); err != nil {
		return err
	}
	var provs struct {
		Providers []model.ProviderStatus `json:"providers"`
	}
	if err := c.Call(ctx, "providers.status", nil, &provs); err != nil {
		return err
	}
	if g.json {
		printJSON(map[string]any{"selected": sel, "providers": provs.Providers})
		return nil
	}
	if sel.Session == nil {
		fmt.Println("No session selected.")
	} else {
		fmt.Printf("Selected (%s):\n%s\n", sel.Reason, describe(*sel.Session, true))
		fmt.Printf("  capabilities: %v\n", sel.Session.Capabilities)
	}
	fmt.Println("\nProviders:")
	for _, p := range provs.Providers {
		printProvider(p)
	}
	return nil
}

func cmdWatch(g globals) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	out := bufio.NewWriter(os.Stdout)
	emit := func(v any) {
		if g.json {
			b, _ := json.Marshal(v)
			out.Write(append(b, '\n'))
		} else {
			switch x := v.(type) {
			case map[string]any:
				if x["type"] == "snapshot" {
					snap := x["snapshot"].(snapshot)
					fmt.Fprintf(out, "== snapshot (revision %d, %d sessions)\n", snap.Revision, len(snap.Sessions))
					for _, s := range snap.Sessions {
						fmt.Fprintln(out, describe(s, snap.SelectedID != nil && *snap.SelectedID == s.ID))
					}
				} else {
					fmt.Fprintf(out, "-- %v\n", x)
				}
			case json.RawMessage:
				var ev struct {
					Revision  uint64         `json:"revision"`
					Type      string         `json:"type"`
					SessionID string         `json:"session_id"`
					Session   *model.Session `json:"session"`
					Reason    string         `json:"reason"`
				}
				_ = json.Unmarshal(x, &ev)
				fmt.Fprintf(out, "[%d] %s %s", ev.Revision, ev.Type, ev.SessionID)
				if ev.Session != nil && ev.Type != "session.removed" {
					fmt.Fprintf(out, " — %s", describe(*ev.Session, false))
				}
				if ev.Reason != "" {
					fmt.Fprintf(out, " (%s)", ev.Reason)
				}
				fmt.Fprintln(out)
			}
		}
		out.Flush()
	}
	backoff := 500 * time.Millisecond
	for ctx.Err() == nil {
		dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		c, err := client.Dial(dctx, g.socket)
		cancel()
		if err != nil {
			emit(map[string]any{"type": "disconnected", "error": err.Error()})
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 10*time.Second)
			continue
		}
		backoff = 500 * time.Millisecond
		var res struct {
			Snapshot snapshot `json:"snapshot"`
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = c.Call(cctx, "subscribe", map[string]any{}, &res)
		cancel()
		if err != nil {
			c.Close()
			emit(map[string]any{"type": "disconnected", "error": err.Error()})
			continue
		}
		emit(map[string]any{"type": "snapshot", "snapshot": res.Snapshot})
	loop:
		for {
			select {
			case <-ctx.Done():
				c.Close()
				return nil
			case n, ok := <-c.Notifications():
				if !ok {
					break loop
				}
				if n.Method == "event" {
					emit(n.Params)
				}
			}
		}
		emit(map[string]any{"type": "disconnected", "error": fmt.Sprint(c.Err())})
	}
	return nil
}

func cmdSettings(g globals, args []string) error {
	if len(args) == 0 || args[0] == "get" {
		var res map[string]any
		if err := call(g, "settings.get", nil, &res); err != nil {
			return err
		}
		printJSON(res)
		return nil
	}
	switch args[0] {
	case "reset":
		var res map[string]any
		if err := call(g, "settings.update", map[string]any{"reset": true}, &res); err != nil {
			return err
		}
		printJSON(res)
		return nil
	case "set":
		p := map[string]any{}
		for _, kv := range args[1:] {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return fmt.Errorf("expected KEY=VALUE, got %q", kv)
			}
			switch k {
			case "show_view_only", "show_stale", "dedup", "local_enabled", "remote_enabled":
				b, err := strconv.ParseBool(v)
				if err != nil {
					return fmt.Errorf("%s needs true or false", k)
				}
				p[k] = b
			case "mode", "preferred_player", "prefer_origin":
				p[k] = v
			default:
				return fmt.Errorf("unknown setting %q", k)
			}
		}
		var res map[string]any
		if err := call(g, "settings.update", p, &res); err != nil {
			return err
		}
		printJSON(res)
		return nil
	}
	return fmt.Errorf("unknown settings subcommand %q", args[0])
}

func cmdJellyfin(g globals, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: jellyfin servers | login SERVER | logout SERVER")
	}
	switch args[0] {
	case "servers":
		var res map[string]any
		if err := call(g, "jellyfin.servers", nil, &res); err != nil {
			return err
		}
		printJSON(res)
		return nil
	case "login":
		fs := flag.NewFlagSet("login", flag.ExitOnError)
		user := fs.String("user", "", "Jellyfin user name")
		pwStdin := fs.Bool("password-stdin", false, "read the password from standard input")
		server, rest := splitPositional(args[1:])
		_ = fs.Parse(rest)
		in := bufio.NewReader(os.Stdin)
		if *user == "" {
			fmt.Fprint(os.Stderr, "User name: ")
			line, _ := in.ReadString('\n')
			*user = strings.TrimSpace(line)
		}
		var pw string
		var err error
		if *pwStdin {
			b, rerr := io.ReadAll(io.LimitReader(in, 4096))
			if rerr != nil {
				return rerr
			}
			pw = strings.TrimRight(string(b), "\r\n")
		} else {
			pw, err = readPassword("Password: ")
			if err != nil {
				return err
			}
		}
		var res map[string]any
		if err := call(g, "jellyfin.login", map[string]any{"server": server, "username": *user, "password": pw}, &res); err != nil {
			return err
		}
		if g.json {
			printJSON(res)
		} else {
			fmt.Printf("Logged in to %v (%v) as %v; token stored in %v.\n", res["server_name"], res["server_version"], res["user_name"], res["credential_backend"])
		}
		return nil
	case "check":
		server, _ := splitPositional(args[1:])
		var res struct {
			ServerURL     string `json:"server_url"`
			ServerName    string `json:"server_name"`
			ServerVersion string `json:"server_version"`
			UserName      string `json:"user_name"`
			WebSocket     struct {
				OK         bool   `json:"ok"`
				HTTPStatus int    `json:"http_status"`
				Detail     string `json:"detail"`
			} `json:"websocket"`
			Clients []struct {
				Client                string   `json:"client"`
				Device                string   `json:"device"`
				NowPlaying            *string  `json:"now_playing"`
				SupportsRemoteControl bool     `json:"supports_remote_control"`
				SupportedCommands     []string `json:"supported_commands"`
				Explanation           string   `json:"explanation"`
			} `json:"clients"`
			Error *model.ErrorInfo `json:"error"`
		}
		if err := call(g, "jellyfin.check", map[string]any{"server": server}, &res); err != nil {
			return err
		}
		if g.json {
			printJSON(res)
			return nil
		}
		fmt.Printf("Server:    %s (%s %s)\n", res.ServerURL, res.ServerName, res.ServerVersion)
		if res.Error != nil {
			fmt.Printf("Error:     %s (%s)\n", res.Error.Message, res.Error.Code)
			return nil
		}
		fmt.Printf("User:      %s\n", res.UserName)
		mark := "FAIL"
		if res.WebSocket.OK {
			mark = "OK"
		}
		fmt.Printf("WebSocket: %s — %s\n", mark, res.WebSocket.Detail)
		if !res.WebSocket.OK {
			fmt.Println("           Remote control needs WebSocket connections. If Jellyfin is behind a reverse proxy,")
			fmt.Println("           enable WebSocket forwarding for the /socket path (see docs/JELLYFIN.md).")
			fmt.Println("           Note: this tests the URL mediasync uses; your phone may use a different one.")
		}
		fmt.Printf("\nApps seen by the server (%d):\n", len(res.Clients))
		for _, c := range res.Clients {
			playing := "idle"
			if c.NowPlaying != nil {
				playing = "playing " + *c.NowPlaying
			}
			fmt.Printf("  %s on %s — %s\n    %s\n", c.Client, c.Device, playing, c.Explanation)
		}
		return nil
	case "logout":
		server, _ := splitPositional(args[1:])
		var res map[string]any
		if err := call(g, "jellyfin.logout", map[string]any{"server": server}, &res); err != nil {
			return err
		}
		printJSON(res)
		return nil
	}
	return fmt.Errorf("unknown jellyfin subcommand %q", args[0])
}

// readPassword reads a line from the terminal with echo disabled.
func readPassword(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return "", errors.New("stdin is not a terminal; use --password-stdin")
	}
	noEcho := *old
	noEcho.Lflag &^= unix.ECHO
	noEcho.Lflag |= unix.ICANON | unix.ISIG
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &noEcho); err != nil {
		return "", err
	}
	defer func() {
		_ = unix.IoctlSetTermios(fd, unix.TCSETS, old)
		fmt.Fprintln(os.Stderr)
	}()
	fmt.Fprint(os.Stderr, prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
