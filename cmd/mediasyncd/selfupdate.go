package main

import (
	"context"
	"os"
	"time"
)

// exitUpdated is the exit status used after the executable was replaced on
// disk (package update, make install). The systemd unit restarts the
// service on it (RestartForceExitStatus=) without marking it as failed.
const exitUpdated = 75

// runningBinary returns the path the service was started from and the
// identity of the file actually running (via /proc/self/exe, which still
// refers to the old file after it was replaced).
func runningBinary() (string, os.FileInfo, error) {
	path, err := os.Executable()
	if err != nil {
		return "", nil, err
	}
	running, err := os.Stat("/proc/self/exe")
	if err != nil {
		return "", nil, err
	}
	return path, running, nil
}

// watchReplaced closes the returned channel once the file at path is no
// longer the running file and a new executable has been in place
// unchanged for one more interval, so a half-written file is never started.
func watchReplaced(ctx context.Context, path string, running os.FileInfo, interval time.Duration) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		var pending os.FileInfo
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			fi, err := os.Stat(path)
			if err != nil || os.SameFile(fi, running) || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
				pending = nil
				continue
			}
			if pending != nil && os.SameFile(pending, fi) &&
				pending.Size() == fi.Size() && pending.ModTime().Equal(fi.ModTime()) {
				close(done)
				return
			}
			pending = fi
		}
	}()
	return done
}

// underSystemd reports whether the service was started by systemd, which
// restarts it after a self-initiated exit. Started by hand, the service
// never exits on its own.
func underSystemd() bool {
	return os.Getenv("INVOCATION_ID") != "" || os.Getenv("NOTIFY_SOCKET") != ""
}
