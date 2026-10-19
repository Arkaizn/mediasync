//go:build integration

package integration

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// wsConn is a minimal RFC 6455 client used to simulate a remote-controllable
// Jellyfin client in integration tests (text frames, ping/pong, close).
type wsConn struct {
	conn net.Conn
	rd   *bufio.Reader
	wmu  sync.Mutex
}

func dialWS(rawURL string, header http.Header) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	host := u.Host
	var conn net.Conn
	switch u.Scheme {
	case "ws":
		if u.Port() == "" {
			host += ":80"
		}
		conn, err = net.DialTimeout("tcp", host, 10*time.Second)
	case "wss":
		if u.Port() == "" {
			host += ":443"
		}
		conn, err = tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", host, &tls.Config{ServerName: u.Hostname()})
	default:
		return nil, fmt.Errorf("unsupported scheme %s", u.Scheme)
	}
	if err != nil {
		return nil, err
	}
	keyb := make([]byte, 16)
	_, _ = rand.Read(keyb)
	key := base64.StdEncoding.EncodeToString(keyb)
	var req strings.Builder
	fmt.Fprintf(&req, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n", u.RequestURI(), u.Host, key)
	for k, vs := range header {
		for _, v := range vs {
			fmt.Fprintf(&req, "%s: %s\r\n", k, v)
		}
	}
	req.WriteString("\r\n")
	if _, err := conn.Write([]byte(req.String())); err != nil {
		conn.Close()
		return nil, err
	}
	rd := bufio.NewReader(conn)
	resp, err := http.ReadResponse(rd, &http.Request{Method: http.MethodGet})
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return nil, fmt.Errorf("websocket handshake failed: HTTP %d", resp.StatusCode)
	}
	h := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	if resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(h[:]) {
		conn.Close()
		return nil, errors.New("bad Sec-WebSocket-Accept")
	}
	return &wsConn{conn: conn, rd: rd}, nil
}

func (w *wsConn) writeFrame(op byte, payload []byte) error {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	var hdr []byte
	hdr = append(hdr, 0x80|op)
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n < 65536:
		hdr = append(hdr, 0x80|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 0x80|127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	mask := make([]byte, 4)
	_, _ = rand.Read(mask)
	hdr = append(hdr, mask...)
	masked := make([]byte, n)
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	_ = w.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := w.conn.Write(append(hdr, masked...))
	return err
}

// WriteText sends a text message.
func (w *wsConn) WriteText(s string) error { return w.writeFrame(0x1, []byte(s)) }

// ReadText returns the next text message, answering pings.
func (w *wsConn) ReadText() (string, error) {
	var msg []byte
	for {
		b0, err := w.rd.ReadByte()
		if err != nil {
			return "", err
		}
		b1, err := w.rd.ReadByte()
		if err != nil {
			return "", err
		}
		op := b0 & 0x0f
		n := uint64(b1 & 0x7f)
		switch n {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(w.rd, b[:]); err != nil {
				return "", err
			}
			n = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(w.rd, b[:]); err != nil {
				return "", err
			}
			n = binary.BigEndian.Uint64(b[:])
		}
		if n > 16<<20 {
			return "", errors.New("frame too large")
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(w.rd, payload); err != nil {
			return "", err
		}
		switch op {
		case 0x8:
			return "", io.EOF
		case 0x9:
			_ = w.writeFrame(0xA, payload)
			continue
		case 0xA:
			continue
		}
		msg = append(msg, payload...)
		if b0&0x80 != 0 {
			return string(msg), nil
		}
	}
}

func (w *wsConn) Close() error {
	_ = w.writeFrame(0x8, []byte{0x03, 0xe8})
	return w.conn.Close()
}
