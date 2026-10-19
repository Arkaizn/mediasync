// Package client is a Go client for the mediasync IPC API.
package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/arkaizn/mediasync/internal/ipc"
)

// Error is an error returned by the service.
type Error struct {
	RPCCode int
	// Code is the stable mediasync error code (e.g. "session_not_found").
	Code    string
	Message string
	Details map[string]any
}

func (e *Error) Error() string {
	if e.Code != "" {
		return e.Code + ": " + e.Message
	}
	return fmt.Sprintf("rpc error %d: %s", e.RPCCode, e.Message)
}

// ErrClosed is returned when the connection is gone.
var ErrClosed = errors.New("connection closed")

// Notification is a raw server notification.
type Notification struct {
	Method string
	Params json.RawMessage
}

type response struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Params json.RawMessage `json:"params"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    *struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		} `json:"data"`
	} `json:"error"`
}

// Client is a connection to the service.
type Client struct {
	conn net.Conn

	wmu     sync.Mutex
	mu      sync.Mutex
	nextID  uint64
	pending map[string]chan response
	notes   chan Notification
	done    chan struct{}
	err     error
}

// Dial connects to the service socket.
func Dial(ctx context.Context, path string) (*Client, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	c := &Client{conn: conn, pending: map[string]chan response{}, notes: make(chan Notification, 1024), done: make(chan struct{})}
	go c.read()
	return c, nil
}

// Close closes the connection.
func (c *Client) Close() error { return c.conn.Close() }

// Done is closed when the connection ends.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err returns why the connection ended.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Notifications delivers server notifications. It is closed when the
// connection ends. If the consumer falls behind by more than 1024
// notifications, the connection is closed.
func (c *Client) Notifications() <-chan Notification { return c.notes }

func (c *Client) read() {
	sc := bufio.NewScanner(c.conn)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	defer func() {
		c.mu.Lock()
		if c.err == nil {
			c.err = sc.Err()
			if c.err == nil {
				c.err = ErrClosed
			}
		}
		for _, ch := range c.pending {
			close(ch)
		}
		c.pending = map[string]chan response{}
		c.mu.Unlock()
		close(c.notes)
		close(c.done)
		c.conn.Close()
	}()
	for sc.Scan() {
		var r response
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			continue
		}
		if len(r.ID) == 0 || string(r.ID) == "null" {
			if r.Method != "" {
				select {
				case c.notes <- Notification{Method: r.Method, Params: r.Params}:
				default:
					c.mu.Lock()
					c.err = errors.New("notification queue overflow")
					c.mu.Unlock()
					return
				}
			}
			continue
		}
		c.mu.Lock()
		ch, ok := c.pending[string(r.ID)]
		delete(c.pending, string(r.ID))
		c.mu.Unlock()
		if ok {
			ch <- r
		}
	}
}

// Call invokes a method and decodes the result into result (may be nil).
func (c *Client) Call(ctx context.Context, method string, params any, result any) error {
	c.mu.Lock()
	select {
	case <-c.done:
		c.mu.Unlock()
		return ErrClosed
	default:
	}
	c.nextID++
	id := strconv.FormatUint(c.nextID, 10)
	ch := make(chan response, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	req := map[string]any{"jsonrpc": ipc.JSONRPCVersion, "id": json.RawMessage(id), "method": method}
	if params != nil {
		req["params"] = params
	}
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err = c.conn.Write(append(b, '\n'))
	c.wmu.Unlock()
	if err != nil {
		return err
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return ErrClosed
		}
		if r.Error != nil {
			e := &Error{RPCCode: r.Error.Code, Message: r.Error.Message}
			if r.Error.Data != nil {
				e.Code, e.Details = r.Error.Data.Code, r.Error.Data.Details
			}
			return e
		}
		if result != nil {
			return json.Unmarshal(r.Result, result)
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	}
}
