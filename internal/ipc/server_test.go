package ipc

import (
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestCloseWhileAccepting closes the server while clients keep connecting.
// Run with -race: Close waits for connection handlers while Serve may be
// accepting a new one.
func TestCloseWhileAccepting(t *testing.T) {
	for i := 0; i < 50; i++ {
		dir, err := os.MkdirTemp("", "ipc")
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "s.sock")
		s := NewServer(path, 1024, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err := s.Listen(); err != nil {
			t.Fatal(err)
		}
		served := make(chan error, 1)
		go func() { served <- s.Serve() }()

		stop := make(chan struct{})
		var wg sync.WaitGroup
		for j := 0; j < 4; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
						_ = c.Close()
					}
				}
			}()
		}
		time.Sleep(time.Duration(i%5) * time.Millisecond)
		s.Close()
		close(stop)
		wg.Wait()
		select {
		case err := <-served:
			if err != nil {
				t.Fatalf("Serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Serve did not return after Close")
		}
		if n := s.Stats()["clients"].(int); n != 0 {
			t.Fatalf("%d connections left after Close", n)
		}
		os.RemoveAll(dir)
	}
}
