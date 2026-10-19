// Package dbustest starts a private dbus-daemon for tests.
package dbustest

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

const configTemplate = `<!DOCTYPE busconfig PUBLIC "-//freedesktop//DTD D-Bus Bus Configuration 1.0//EN"
 "http://www.freedesktop.org/standards/dbus/1.0/busconfig.dtd">
<busconfig>
  <type>session</type>
  <listen>unix:path=%s</listen>
  <auth>EXTERNAL</auth>
  <policy context="default">
    <allow send_destination="*" eavesdrop="true"/>
    <allow eavesdrop="true"/>
    <allow own="*"/>
  </policy>
</busconfig>
`

// Bus is a private session bus.
type Bus struct {
	Address string
	cmd     *exec.Cmd
}

// Start launches a private dbus-daemon. The test is skipped if dbus-daemon
// is not installed. The daemon is stopped on test cleanup.
func Start(t testing.TB) *Bus {
	t.Helper()
	bin, err := exec.LookPath("dbus-daemon")
	if err != nil {
		t.Skip("dbus-daemon not installed; skipping D-Bus test")
	}
	dir := t.TempDir()
	sock := filepath.Join(dir, "bus")
	cfg := filepath.Join(dir, "bus.conf")
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf(configTemplate, sock)), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "--config-file="+cfg, "--nofork", "--print-address=1", "--nopidfile")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("MEDIASYNC_TEST_DBUS_VERBOSE") != "" {
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start dbus-daemon: %v", err)
	}
	addrCh := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(out).ReadString('\n')
		addrCh <- strings.TrimSpace(line)
	}()
	var addr string
	select {
	case addr = <-addrCh:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("dbus-daemon did not report its address")
	}
	b := &Bus{Address: addr, cmd: cmd}
	t.Cleanup(b.Stop)
	return b
}

// Stop terminates the daemon.
func (b *Bus) Stop() {
	if b.cmd != nil && b.cmd.Process != nil {
		_ = b.cmd.Process.Kill()
		_ = b.cmd.Wait()
		b.cmd = nil
	}
}

// Connect opens a new connection to the private bus.
func (b *Bus) Connect() (*dbus.Conn, error) {
	conn, err := dbus.Dial(b.Address)
	if err != nil {
		return nil, err
	}
	if err := conn.Auth(nil); err != nil {
		conn.Close()
		return nil, err
	}
	if err := conn.Hello(); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}
