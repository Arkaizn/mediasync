// Package ipc implements the local transport: JSON-RPC 2.0 messages,
// one per line, over a Unix domain socket.
//
// Security: the socket lives in a 0700 directory, has mode 0600, and every
// connection's peer credentials (SO_PEERCRED) must match the service's UID.
// Nothing listens on the network.
package ipc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/arkaizn/mediasync/internal/model"
)

// Limits.
const (
	MaxMessageBytes   = 1 << 20
	outQueueSize      = 1024
	maxInFlight       = 16
	writeTimeout      = 5 * time.Second
	JSONRPCVersion    = "2.0"
	NotificationEvent = "event"
)

// JSON-RPC error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
	// CodeApplication is used for all service errors; data.code carries the
	// stable mediasync error code.
	CodeApplication = -32000
)

// Request is a JSON-RPC request or notification.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a JSON-RPC response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// Notification is a server-to-client message without id.
type Notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int        `json:"code"`
	Message string     `json:"message"`
	Data    *ErrorData `json:"data,omitempty"`
}

// ErrorData carries the stable mediasync error code.
type ErrorData struct {
	Code    model.ErrorCode `json:"code"`
	Details map[string]any  `json:"details,omitempty"`
}

// InvalidParams builds an invalid-params error.
func InvalidParams(format string, args ...any) error {
	return &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf(format, args...),
		Data: &ErrorData{Code: model.ErrInvalidArgument}}
}

func (e *RPCError) Error() string { return e.Message }

func toRPCError(err error) *RPCError {
	var re *RPCError
	if errors.As(err, &re) {
		return re
	}
	me := model.AsError(err)
	code := CodeApplication
	if me.Code == model.ErrInvalidArgument {
		code = CodeInvalidParams
	}
	msg := me.Message
	if msg == "" {
		msg = string(me.Code)
	}
	return &RPCError{Code: code, Message: msg, Data: &ErrorData{Code: me.Code, Details: me.Data}}
}

// Deferred lets a handler run code after its response has been queued
// (e.g. start streaming notifications that must follow the response).
type Deferred struct {
	Result any
	After  func()
}

// MethodFunc handles one method call.
type MethodFunc func(ctx context.Context, c *Conn, params json.RawMessage) (any, error)

// Server is the IPC server.
type Server struct {
	log        *slog.Logger
	path       string
	maxClients int

	mu       sync.Mutex
	methods  map[string]MethodFunc
	ln       *net.UnixListener
	conns    map[*Conn]struct{}
	closed   bool
	nextID   atomic.Uint64
	accepted atomic.Uint64
	rejected atomic.Uint64
	wg       sync.WaitGroup
}

// NewServer creates a server for the socket path.
func NewServer(path string, maxClients int, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{log: log.With("component", "ipc"), path: path, maxClients: maxClients,
		methods: map[string]MethodFunc{}, conns: map[*Conn]struct{}{}}
}

// Handle registers a method.
func (s *Server) Handle(method string, f MethodFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.methods[method] = f
}

// Methods returns registered method names.
func (s *Server) Methods() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.methods))
	for m := range s.methods {
		out = append(out, m)
	}
	return out
}

// Path returns the socket path.
func (s *Server) Path() string { return s.path }

// Listen creates the socket. It refuses to replace a socket that has a live
// server behind it.
func (s *Server) Listen() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return model.Wrap(model.ErrInternal, err, "cannot create socket directory "+dir)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return model.Wrap(model.ErrInternal, err, "cannot stat socket directory")
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return model.Errorf(model.ErrInternal, "socket directory %s is owned by another user", dir)
	}
	if !fi.IsDir() {
		return model.Errorf(model.ErrInternal, "%s is not a directory", dir)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return model.Wrap(model.ErrInternal, err, "cannot restrict socket directory permissions")
	}
	if _, err := os.Lstat(s.path); err == nil {
		if c, err := net.DialTimeout("unix", s.path, time.Second); err == nil {
			c.Close()
			return model.Errorf(model.ErrInternal, "another instance is already listening on %s", s.path)
		}
		if err := os.Remove(s.path); err != nil {
			return model.Wrap(model.ErrInternal, err, "cannot remove stale socket")
		}
	}
	old := syscall.Umask(0o177)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: s.path, Net: "unix"})
	syscall.Umask(old)
	if err != nil {
		return model.Wrap(model.ErrInternal, err, "cannot listen on "+s.path)
	}
	ln.SetUnlinkOnClose(true)
	if err := os.Chmod(s.path, 0o600); err != nil {
		ln.Close()
		return model.Wrap(model.ErrInternal, err, "cannot restrict socket permissions")
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	return nil
}

// Serve accepts connections until Close.
func (s *Server) Serve() error {
	s.mu.Lock()
	ln := s.ln
	s.mu.Unlock()
	if ln == nil {
		return errors.New("ipc: Listen not called")
	}
	for {
		uc, err := ln.AcceptUnix()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		if err := checkPeer(uc); err != nil {
			s.rejected.Add(1)
			s.log.Warn("rejected connection", "err", err)
			_ = uc.Close()
			continue
		}
		s.mu.Lock()
		if len(s.conns) >= s.maxClients {
			s.mu.Unlock()
			s.rejected.Add(1)
			s.log.Warn("too many clients; rejecting connection", "max", s.maxClients)
			_ = writeLine(uc, Notification{JSONRPC: JSONRPCVersion, Method: "error", Params: map[string]any{"code": "too_many_clients"}})
			_ = uc.Close()
			continue
		}
		c := newConn(s, uc)
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		s.accepted.Add(1)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			c.serve()
		}()
	}
}

// checkPeer verifies the connecting process runs as our UID.
func checkPeer(uc *net.UnixConn) error {
	raw, err := uc.SyscallConn()
	if err != nil {
		return err
	}
	var cred *syscall.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if cerr != nil {
		return cerr
	}
	if int(cred.Uid) != os.Getuid() {
		return fmt.Errorf("peer uid %d does not match service uid %d", cred.Uid, os.Getuid())
	}
	return nil
}

// Close stops the server and disconnects all clients.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	ln := s.ln
	conns := make([]*Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
	for _, c := range conns {
		c.Close()
	}
	s.wg.Wait()
}

// Stats returns connection statistics.
func (s *Server) Stats() map[string]any {
	s.mu.Lock()
	n := len(s.conns)
	s.mu.Unlock()
	return map[string]any{"clients": n, "accepted": s.accepted.Load(), "rejected": s.rejected.Load(), "socket": s.path, "max_clients": s.maxClients}
}

func (s *Server) remove(c *Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

func (s *Server) method(name string) (MethodFunc, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.methods[name]
	return f, ok
}

// Conn is one client connection.
type Conn struct {
	s      *Server
	uc     *net.UnixConn
	id     uint64
	out    chan any
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	closed  bool
	onClose []func()
	// Values is per-connection state for method handlers.
	values map[string]any
	// Name is the client name announced in hello.
	name string
}

func newConn(s *Server, uc *net.UnixConn) *Conn {
	ctx, cancel := context.WithCancel(context.Background())
	return &Conn{s: s, uc: uc, id: s.nextID.Add(1), out: make(chan any, outQueueSize), ctx: ctx, cancel: cancel, values: map[string]any{}}
}

// ID returns the connection id.
func (c *Conn) ID() uint64 { return c.id }

// Context is cancelled when the connection closes.
func (c *Conn) Context() context.Context { return c.ctx }

// SetName records the client name.
func (c *Conn) SetName(n string) { c.mu.Lock(); c.name = n; c.mu.Unlock() }

// Value returns per-connection state.
func (c *Conn) Value(k string) any { c.mu.Lock(); defer c.mu.Unlock(); return c.values[k] }

// SetValue stores per-connection state.
func (c *Conn) SetValue(k string, v any) { c.mu.Lock(); c.values[k] = v; c.mu.Unlock() }

// OnClose registers a cleanup function.
func (c *Conn) OnClose(f func()) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		f()
		return
	}
	c.onClose = append(c.onClose, f)
	c.mu.Unlock()
}

// Notify queues a notification. If the client does not keep up, the
// connection is closed; the client must reconnect and take a new snapshot.
func (c *Conn) Notify(method string, params any) {
	c.send(Notification{JSONRPC: JSONRPCVersion, Method: method, Params: params})
}

func (c *Conn) send(msg any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	select {
	case c.out <- msg:
	default:
		c.s.log.Warn("client too slow; closing connection", "conn", c.id, "client", c.name)
		go c.Close()
	}
}

// Close closes the connection.
func (c *Conn) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	fns := c.onClose
	c.onClose = nil
	c.mu.Unlock()
	c.cancel()
	_ = c.uc.Close()
	for _, f := range fns {
		f()
	}
	c.s.remove(c)
}

func writeLine(uc *net.UnixConn, msg any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_ = uc.SetWriteDeadline(time.Now().Add(writeTimeout))
	_, err = uc.Write(append(b, '\n'))
	return err
}

func (c *Conn) writer() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case msg := <-c.out:
			if err := writeLine(c.uc, msg); err != nil {
				c.Close()
				return
			}
		}
	}
}

func (c *Conn) serve() {
	defer c.Close()
	go c.writer()
	sc := bufio.NewScanner(c.uc)
	sc.Buffer(make([]byte, 64<<10), MaxMessageBytes)
	sem := make(chan struct{}, maxInFlight)
	var wg sync.WaitGroup
	defer wg.Wait()
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			c.send(Response{JSONRPC: JSONRPCVersion, ID: json.RawMessage("null"), Error: &RPCError{Code: CodeParseError, Message: "parse error"}})
			continue
		}
		if req.JSONRPC != JSONRPCVersion || req.Method == "" {
			c.send(Response{JSONRPC: JSONRPCVersion, ID: idOrNull(req.ID), Error: &RPCError{Code: CodeInvalidRequest, Message: "invalid request"}})
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-c.ctx.Done():
			return
		}
		wg.Add(1)
		go func(req Request) {
			defer wg.Done()
			defer func() { <-sem }()
			c.dispatch(req)
		}(req)
	}
	if err := sc.Err(); err != nil && !errors.Is(err, net.ErrClosed) {
		if errors.Is(err, bufio.ErrTooLong) {
			c.s.log.Warn("message too large; closing connection", "conn", c.id)
			_ = writeLine(c.uc, Response{JSONRPC: JSONRPCVersion, ID: json.RawMessage("null"),
				Error: &RPCError{Code: CodeInvalidRequest, Message: "message too large"}})
		}
	}
}

func idOrNull(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

func (c *Conn) dispatch(req Request) {
	defer func() {
		if r := recover(); r != nil {
			c.s.log.Error("panic in method handler", "method", req.Method, "panic", r)
			if len(req.ID) > 0 {
				c.send(Response{JSONRPC: JSONRPCVersion, ID: req.ID, Error: &RPCError{Code: CodeInternalError, Message: "internal error", Data: &ErrorData{Code: model.ErrInternal}}})
			}
		}
	}()
	f, ok := c.s.method(req.Method)
	if !ok {
		if len(req.ID) > 0 {
			c.send(Response{JSONRPC: JSONRPCVersion, ID: req.ID, Error: &RPCError{Code: CodeMethodNotFound, Message: "method not found: " + req.Method}})
		}
		return
	}
	// Method names only: parameters can contain passwords.
	c.s.log.Debug("request", "conn", c.id, "method", req.Method)
	res, err := f(c.ctx, c, req.Params)
	if len(req.ID) == 0 {
		if d, ok := res.(Deferred); ok && d.After != nil {
			d.After()
		}
		return // notification: no response
	}
	if err != nil {
		c.send(Response{JSONRPC: JSONRPCVersion, ID: req.ID, Error: toRPCError(err)})
		return
	}
	var after func()
	if d, ok := res.(Deferred); ok {
		res, after = d.Result, d.After
	}
	if res == nil {
		res = struct{}{}
	}
	c.send(Response{JSONRPC: JSONRPCVersion, ID: req.ID, Result: res})
	if after != nil {
		after()
	}
}

// DecodeParams decodes params strictly (unknown fields rejected). Empty
// params decode to the zero value.
func DecodeParams(raw json.RawMessage, v any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	dec := json.NewDecoder(bytesReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return InvalidParams("invalid params: %s", err.Error())
	}
	return nil
}

func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }
