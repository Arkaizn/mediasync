package secrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/arkaizn/mediasync/internal/model"
	"github.com/arkaizn/mediasync/internal/redact"
	"github.com/arkaizn/mediasync/internal/testutil/dbustest"
	"github.com/arkaizn/mediasync/internal/testutil/fakesecrets"
)

var attrs = Attributes{Kind: "jellyfin-token", Instance: "home", Server: "https://jf.example", User: "alice"}

func exercise(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()
	if err := s.Check(ctx); err != nil {
		t.Fatalf("check: %v", err)
	}
	if _, err := s.Get(ctx, attrs); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
	if err := s.Set(ctx, attrs, "label", "tok-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, attrs, "label", "tok-2"); err != nil { // replace
		t.Fatal(err)
	}
	other := attrs
	other.Instance = "work"
	if err := s.Set(ctx, other, "label", "tok-work"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, attrs)
	if err != nil || got.Reveal() != "tok-2" {
		t.Fatalf("get: %q %v", got.Reveal(), err)
	}
	if err := s.Delete(ctx, attrs); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, attrs); err != nil { // idempotent
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, attrs); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if got, _ := s.Get(ctx, other); got.Reveal() != "tok-work" {
		t.Fatal("other entry affected")
	}
}

func TestMemory(t *testing.T) { exercise(t, NewMemory()) }

func TestFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	f := NewFile(filepath.Join(dir, "credentials.json"))
	exercise(t, f)
	fi, err := os.Stat(f.Path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode %o", fi.Mode().Perm())
	}
	di, _ := os.Stat(dir)
	if di.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %o", di.Mode().Perm())
	}
}

func TestFileRejectsLoosePermissionsAndCorruption(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "credentials.json")
	f := NewFile(p)
	if err := f.Set(context.Background(), attrs, "", "x"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := f.Get(context.Background(), attrs)
	if model.CodeOf(err) != model.ErrCredentialStore || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("want permission error, got %v", err)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = f.Get(context.Background(), attrs)
	if model.CodeOf(err) != model.ErrCredentialStore || strings.Contains(err.Error(), "{not") {
		t.Fatalf("want corrupt error without content, got %v", err)
	}
	// Loose directory permissions are rejected too.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.Check(context.Background()); model.CodeOf(err) != model.ErrCredentialStore {
		t.Fatalf("want dir permission error, got %v", err)
	}
}

func startFake(t *testing.T) (*dbustest.Bus, *fakesecrets.Service, *SecretService) {
	bus := dbustest.Start(t)
	conn, err := bus.Connect()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	fake, err := fakesecrets.Start(conn)
	if err != nil {
		t.Fatal(err)
	}
	ss := NewSecretService()
	ss.Connect = bus.Connect
	ss.PromptTimeout = 2 * time.Second
	t.Cleanup(ss.Close)
	return bus, fake, ss
}

func TestSecretService(t *testing.T) {
	_, fake, ss := startFake(t)
	exercise(t, ss)
	if v := fake.Items()["work|https://jf.example"]; v != "tok-work" {
		t.Errorf("stored items: %v", fake.Items())
	}
}

func TestSecretServiceLockedItemsPrompt(t *testing.T) {
	_, fake, ss := startFake(t)
	ctx := context.Background()
	if err := ss.Set(ctx, attrs, "l", "tok"); err != nil {
		t.Fatal(err)
	}
	fake.LockAll()
	got, err := ss.Get(ctx, attrs)
	if err != nil || got.Reveal() != "tok" {
		t.Fatalf("get locked: %v", err)
	}
	if fake.Prompts() != 1 {
		t.Errorf("prompts %d", fake.Prompts())
	}
	fake.LockAll()
	fake.SetDismissPrompts(true)
	if _, err := ss.Get(ctx, attrs); model.CodeOf(err) != model.ErrCredentialStore || !strings.Contains(err.Error(), "dismissed") {
		t.Fatalf("dismissed: %v", err)
	}
}

func TestSecretServiceUnavailable(t *testing.T) {
	bus := dbustest.Start(t) // no secret service on this bus
	ss := NewSecretService()
	ss.Connect = bus.Connect
	defer ss.Close()
	err := ss.Check(context.Background())
	if model.CodeOf(err) != model.ErrCredentialStoreUnavailable {
		t.Fatalf("want unavailable, got %v", err)
	}
	if _, err := ss.Get(context.Background(), attrs); model.CodeOf(err) != model.ErrCredentialStoreUnavailable {
		t.Fatalf("get: %v", err)
	}
	// No bus at all.
	ss2 := NewSecretService()
	ss2.Connect = func() (*dbus.Conn, error) { return nil, errors.New("no bus") }
	if err := ss2.Check(context.Background()); model.CodeOf(err) != model.ErrCredentialStoreUnavailable {
		t.Fatalf("no bus: %v", err)
	}
}

func TestSecretServiceNoDefaultCollection(t *testing.T) {
	_, fake, ss := startFake(t)
	fake.SetNoDefault(true)
	err := ss.Set(context.Background(), attrs, "l", redact.Secret("tok"))
	if model.CodeOf(err) != model.ErrCredentialStore {
		t.Fatalf("want credential store error, got %v", err)
	}
}

func TestErrorsNeverContainSecret(t *testing.T) {
	m := NewMemory()
	m.Fail = errors.New("backend exploded token=abcd")
	err := storeError("memory", m.Fail, "read")
	if strings.Contains(err.Error(), "abcd") {
		t.Fatalf("secret leaked: %v", err)
	}
}
