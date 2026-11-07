package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeExe(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil { // like pacman and install(1)
		t.Fatal(err)
	}
}

func TestWatchReplacedDetectsNewExecutable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mediasyncd")
	writeExe(t, path, "old")
	running, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	replaced := watchReplaced(ctx, path, running, 20*time.Millisecond)

	select {
	case <-replaced:
		t.Fatal("reported a replacement while the file is unchanged")
	case <-time.After(150 * time.Millisecond):
	}

	writeExe(t, path, "new")
	select {
	case <-replaced:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement not detected")
	}
}

func TestWatchReplacedIgnoresMissingAndNonExecutable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mediasyncd")
	writeExe(t, path, "old")
	running, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	replaced := watchReplaced(ctx, path, running, 20*time.Millisecond)

	if err := os.Remove(path); err != nil { // uninstall in progress
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not yet executable"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-replaced:
		t.Fatal("restarted for a missing or non-executable file")
	case <-time.After(200 * time.Millisecond):
	}
	cancel()
}

func TestRunningBinaryIsTheTestExecutable(t *testing.T) {
	path, running, err := runningBinary()
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(fi, running) {
		t.Fatalf("%s is not the running binary", path)
	}
}
