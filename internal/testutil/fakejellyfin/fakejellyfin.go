// Package fakejellyfin is an in-process HTTP server that mimics the subset
// of the Jellyfin 12.x API used by mediasync, including behaviour verified
// against a real Jellyfin 12.2 server:
//
//   - only "Authorization: MediaBrowser ... Token=..." authenticates; the
//     legacy X-Emby-Token header and api_key query parameter get 401;
//   - playstate/general command endpoints answer 204 for any existing
//     session, even one without a control connection (the command is then
//     silently not delivered); unknown sessions get 404; unknown commands 400;
//   - the server extrapolates PositionTicks for unpaused sessions;
//   - POST /Sessions/Logout revokes the calling token.
package fakejellyfin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client is a simulated Jellyfin client device with a playback session.
type Client struct {
	SessionID  string
	UserID     string
	UserName   string
	Client     string
	DeviceName string
	DeviceID   string
	AppVersion string
	RemoteAddr string
	// Controllable simulates a live WebSocket control connection.
	Controllable      bool
	SupportedCommands []string
	// ApplyCommands makes the client react to delivered commands.
	ApplyCommands bool

	ItemID       string
	Title        string
	Album        string
	AlbumID      string
	AlbumArtist  string
	Artists      []string
	RunTimeTicks int64
	ImageTag     string

	PositionTicks int64
	Paused        bool
	CanSeek       bool
	Volume        int
	LastCheckIn   time.Time
	checkInAt     time.Time // local time of last position update
	Stopped       bool
	queue         []string
	queueIdx      int

	Received []string
}

// Server is the fake Jellyfin server.
type Server struct {
	*httptest.Server

	mu       sync.Mutex
	ServerID string
	Name     string
	Version  string
	users    map[string]string // name -> password
	userIDs  map[string]string
	tokens   map[string]string // token -> user name
	clients  map[string]*Client
	n        int
	// Requests records method+URL (path?query) of every request.
	requests []string
	headers  []http.Header
	// Down makes every request fail with a closed connection.
	down bool
	// Fail500 makes /Sessions return 500.
	fail500 bool
}

// New starts a fake server with one user.
func New(user, password string) *Server {
	s := &Server{
		ServerID: "e6a658a0390740f3ab10638a00b6761f", Name: "fakefin", Version: "12.2.0",
		users: map[string]string{user: password}, userIDs: map[string]string{user: "1a62e05105e2486ba0877d0f29a72e3e"},
		tokens: map[string]string{}, clients: map[string]*Client{},
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// NewTLS starts a fake server with a self-signed certificate.
func NewTLS(user, password string) *Server {
	s := &Server{
		ServerID: "e6a658a0390740f3ab10638a00b6761f", Name: "fakefin", Version: "12.2.0",
		users: map[string]string{user: password}, userIDs: map[string]string{user: "1a62e05105e2486ba0877d0f29a72e3e"},
		tokens: map[string]string{}, clients: map[string]*Client{},
	}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(s.handle))
	return s
}

// UserID returns the id of a user.
func (s *Server) UserID(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.userIDs[name]
}

// AddClient registers a playing client. Missing fields get defaults.
func (s *Server) AddClient(c *Client) *Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	if c.SessionID == "" {
		c.SessionID = fmt.Sprintf("%032x", s.n)
	}
	if c.AppVersion == "" {
		c.AppVersion = "1.0"
	}
	if c.LastCheckIn.IsZero() {
		c.LastCheckIn = time.Now().UTC()
	}
	c.checkInAt = time.Now()
	if c.RemoteAddr == "" {
		c.RemoteAddr = "192.168.1.50"
	}
	if c.queue == nil && c.Title != "" {
		c.queue = []string{c.Title, c.Title + " II", c.Title + " III"}
	}
	s.clients[c.SessionID] = c
	return c
}

// Update mutates a client under the server lock.
func (s *Server) Update(id string, f func(c *Client)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.clients[id]; ok {
		s.syncPos(c)
		f(c)
	}
}

// RemoveClient drops a session (e.g. its WebSocket closed).
func (s *Server) RemoveClient(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clients, id)
}

// ClientState returns a copy of a client.
func (s *Server) ClientState(id string) Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.clients[id]
	if c == nil {
		return Client{}
	}
	s.syncPos(c)
	cp := *c
	cp.Received = append([]string(nil), c.Received...)
	return cp
}

// SetDown simulates an unreachable server.
func (s *Server) SetDown(v bool) { s.mu.Lock(); s.down = v; s.mu.Unlock() }

// SetFail500 makes /Sessions fail.
func (s *Server) SetFail500(v bool) { s.mu.Lock(); s.fail500 = v; s.mu.Unlock() }

// Requests returns recorded request lines.
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

// Headers returns recorded request headers.
func (s *Server) Headers() []http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]http.Header(nil), s.headers...)
}

// Tokens returns the number of valid tokens.
func (s *Server) Tokens() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tokens)
}

// syncPos extrapolates the position like the real server does.
func (s *Server) syncPos(c *Client) {
	now := time.Now()
	if !c.Paused && !c.Stopped {
		c.PositionTicks += now.Sub(c.checkInAt).Milliseconds() * 10_000
		if c.RunTimeTicks > 0 && c.PositionTicks > c.RunTimeTicks {
			c.PositionTicks = c.RunTimeTicks
		}
	}
	c.checkInAt = now
}

var authParam = regexp.MustCompile(`(\w+)="([^"]*)"`)

func parseAuth(h string) map[string]string {
	out := map[string]string{}
	if !strings.HasPrefix(h, "MediaBrowser ") {
		return out
	}
	for _, m := range authParam.FindAllStringSubmatch(h, -1) {
		out[m[1]] = m[2]
	}
	return out
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, r.Method+" "+r.URL.RequestURI())
	s.headers = append(s.headers, r.Header.Clone())
	down := s.down
	s.mu.Unlock()
	if down {
		hj, ok := w.(http.Hijacker)
		if ok {
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	auth := parseAuth(r.Header.Get("Authorization"))
	path := r.URL.Path

	switch {
	case path == "/System/Info/Public" && r.Method == http.MethodGet:
		writeJSON(w, map[string]any{"ServerName": s.Name, "Version": s.Version, "ProductName": "Jellyfin Server", "Id": s.ServerID, "StartupWizardCompleted": true, "LocalAddress": "http://172.17.0.2:8096"})
		return
	case path == "/Users/AuthenticateByName" && r.Method == http.MethodPost:
		var body struct{ Username, Pw string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		if auth["DeviceId"] == "" {
			http.Error(w, "missing device id", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		pw, ok := s.users[body.Username]
		if !ok || pw != body.Pw {
			s.mu.Unlock()
			http.Error(w, "Error processing request.", http.StatusUnauthorized)
			return
		}
		s.n++
		tok := fmt.Sprintf("tok%030d", s.n)
		s.tokens[tok] = body.Username
		uid := s.userIDs[body.Username]
		s.mu.Unlock()
		writeJSON(w, map[string]any{
			"User":        map[string]any{"Name": body.Username, "Id": uid, "ServerId": s.ServerID},
			"SessionInfo": map[string]any{},
			"AccessToken": tok,
			"ServerId":    s.ServerID,
		})
		return
	}

	// Authenticated endpoints: only the Authorization header counts.
	s.mu.Lock()
	user, ok := s.tokens[auth["Token"]]
	s.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	switch {
	case path == "/Users/Me" && r.Method == http.MethodGet:
		writeJSON(w, map[string]any{"Name": user, "Id": s.UserID(user), "ServerId": s.ServerID})
	case path == "/Sessions/Logout" && r.Method == http.MethodPost:
		s.mu.Lock()
		delete(s.tokens, auth["Token"])
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case path == "/Sessions" && r.Method == http.MethodGet:
		s.mu.Lock()
		fail := s.fail500
		s.mu.Unlock()
		if fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		writeJSON(w, s.sessionsJSON())
	case strings.HasPrefix(path, "/Sessions/") && r.Method == http.MethodPost:
		s.command(w, r, strings.TrimPrefix(path, "/Sessions/"))
	default:
		http.NotFound(w, r)
	}
}

var playstateCommands = map[string]bool{"Stop": true, "Pause": true, "Unpause": true, "NextTrack": true, "PreviousTrack": true, "Seek": true, "Rewind": true, "FastForward": true, "PlayPause": true}

func (s *Server) command(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(rest, "/")
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clients[parts[0]]
	if len(parts) >= 3 && parts[1] == "Playing" {
		if !playstateCommands[parts[2]] {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if !ok {
			http.Error(w, "Error processing request.", http.StatusNotFound)
			return
		}
		cmd := parts[2]
		if cmd == "Seek" {
			cmd += "(" + r.URL.Query().Get("seekPositionTicks") + ")"
		}
		s.deliver(c, cmd, r.URL.Query())
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if len(parts) == 2 && parts[1] == "Command" {
		if !ok {
			http.Error(w, "Error processing request.", http.StatusNotFound)
			return
		}
		var body struct {
			Name      string
			Arguments map[string]string
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.deliver(c, body.Name+"("+body.Arguments["Volume"]+")", nil)
		if c.Controllable && c.ApplyCommands && body.Name == "SetVolume" {
			if v, err := strconv.Atoi(body.Arguments["Volume"]); err == nil {
				c.Volume = v
			}
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.NotFound(w, r)
}

// deliver records and applies a command if the client has a control channel.
func (s *Server) deliver(c *Client, cmd string, q map[string][]string) {
	if !c.Controllable {
		return // the real server answers 204 but nothing reaches the client
	}
	c.Received = append(c.Received, cmd)
	if !c.ApplyCommands {
		return
	}
	s.syncPos(c)
	now := time.Now().UTC()
	switch {
	case cmd == "Pause":
		c.Paused = true
	case cmd == "Unpause":
		c.Paused = false
	case cmd == "PlayPause":
		c.Paused = !c.Paused
	case cmd == "Stop":
		c.Stopped = true
	case cmd == "NextTrack" && c.queueIdx+1 < len(c.queue):
		c.queueIdx++
		c.Title = c.queue[c.queueIdx]
		c.ItemID = fmt.Sprintf("item%d", c.queueIdx)
		c.PositionTicks = 0
	case cmd == "PreviousTrack" && c.queueIdx > 0:
		c.queueIdx--
		c.Title = c.queue[c.queueIdx]
		c.ItemID = fmt.Sprintf("item%d", c.queueIdx)
		c.PositionTicks = 0
	case strings.HasPrefix(cmd, "Seek("):
		if t, err := strconv.ParseInt(q["seekPositionTicks"][0], 10, 64); err == nil {
			c.PositionTicks = t
		}
	}
	c.LastCheckIn = now
}

func (s *Server) sessionsJSON() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []map[string]any{}
	for _, c := range s.clients {
		s.syncPos(c)
		cmds := c.SupportedCommands
		if cmds == nil || !c.Controllable {
			cmds = []string{}
		}
		sess := map[string]any{
			"Id": c.SessionID, "UserId": c.UserID, "UserName": c.UserName,
			"Client": c.Client, "DeviceName": c.DeviceName, "DeviceId": c.DeviceID,
			"ApplicationVersion": c.AppVersion, "RemoteEndPoint": c.RemoteAddr, "ServerId": s.ServerID,
			"IsActive": true, "SupportsMediaControl": c.Controllable, "SupportsRemoteControl": c.Controllable,
			"SupportedCommands": cmds, "PlayableMediaTypes": []string{"Audio"},
			"LastActivityDate":    c.LastCheckIn.Format(time.RFC3339Nano),
			"LastPlaybackCheckIn": c.LastCheckIn.Format(time.RFC3339Nano),
			"PlayState": map[string]any{
				"PositionTicks": c.PositionTicks, "CanSeek": c.CanSeek, "IsPaused": c.Paused, "IsMuted": false,
				"VolumeLevel": c.Volume, "RepeatMode": "RepeatNone", "PlaybackOrder": "Default", "PlayMethod": "DirectPlay",
			},
		}
		if !c.Stopped && c.ItemID != "" {
			item := map[string]any{
				"Id": c.ItemID, "Name": c.Title, "Album": c.Album, "AlbumId": c.AlbumID, "AlbumArtist": c.AlbumArtist,
				"Artists": c.Artists, "RunTimeTicks": c.RunTimeTicks, "Type": "Audio", "MediaType": "Audio",
				"ImageTags": map[string]string{},
			}
			if c.ImageTag != "" {
				item["AlbumPrimaryImageTag"] = c.ImageTag
			}
			sess["NowPlayingItem"] = item
		}
		out = append(out, sess)
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}
