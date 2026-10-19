package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/arkaizn/mediasync/internal/model"
	"github.com/arkaizn/mediasync/internal/redact"
)

// File stores secrets in a JSON file with mode 0600 inside a 0700
// directory. The file is refused if its permissions are looser or it is
// not owned by the current user.
type File struct {
	Path string
	mu   sync.Mutex
}

type fileEntry struct {
	Kind     string `json:"kind"`
	Instance string `json:"instance"`
	Server   string `json:"server"`
	User     string `json:"user,omitempty"`
	Label    string `json:"label,omitempty"`
	Secret   string `json:"secret"`
}

type fileData struct {
	Version int         `json:"version"`
	Entries []fileEntry `json:"entries"`
}

// NewFile returns a file store at path.
func NewFile(path string) *File { return &File{Path: path} }

func (f *File) Backend() string { return "file" }

// Check verifies that the directory is usable and the file (if present) has
// safe permissions.
func (f *File) Check(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, err := f.load()
	return err
}

func checkPerms(path string, fi fs.FileInfo, maxPerm fs.FileMode) error {
	if fi.Mode().Perm()&^maxPerm != 0 {
		return model.Errorf(model.ErrCredentialStore,
			"%s has permissions %o; refusing to use it (expected at most %o)", path, fi.Mode().Perm(), maxPerm)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return model.Errorf(model.ErrCredentialStore, "%s is not owned by the current user", path)
	}
	return nil
}

func (f *File) load() (*fileData, error) {
	d := &fileData{Version: 1}
	dir := filepath.Dir(f.Path)
	if fi, err := os.Stat(dir); err == nil {
		if err := checkPerms(dir, fi, 0o700); err != nil {
			return nil, err
		}
	}
	fh, err := os.Open(f.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return d, nil
	}
	if err != nil {
		return nil, storeError("file", err, "read")
	}
	defer fh.Close()
	fi, err := fh.Stat()
	if err != nil {
		return nil, storeError("file", err, "read")
	}
	if err := checkPerms(f.Path, fi, 0o600); err != nil {
		return nil, err
	}
	if err := json.NewDecoder(fh).Decode(d); err != nil {
		return nil, model.Errorf(model.ErrCredentialStore, "%s is corrupt (invalid JSON)", f.Path)
	}
	return d, nil
}

func (f *File) save(d *fileData) error {
	dir := filepath.Dir(f.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return storeError("file", err, "write")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return storeError("file", err, "write")
	}
	tmp, err := os.CreateTemp(dir, ".credentials-*")
	if err != nil {
		return storeError("file", err, "write")
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return storeError("file", err, "write")
	}
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(d); err != nil {
		tmp.Close()
		return storeError("file", err, "write")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return storeError("file", err, "write")
	}
	if err := tmp.Close(); err != nil {
		return storeError("file", err, "write")
	}
	if err := os.Rename(tmp.Name(), f.Path); err != nil {
		return storeError("file", err, "write")
	}
	return nil
}

func match(e fileEntry, a Attributes) bool {
	return e.Kind == a.Kind && e.Instance == a.Instance && e.Server == a.Server
}

func (f *File) Get(_ context.Context, a Attributes) (redact.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, err := f.load()
	if err != nil {
		return "", err
	}
	for _, e := range d.Entries {
		if match(e, a) {
			return redact.Secret(e.Secret), nil
		}
	}
	return "", ErrNotFound
}

func (f *File) Set(_ context.Context, a Attributes, label string, s redact.Secret) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, err := f.load()
	if err != nil {
		return err
	}
	out := d.Entries[:0]
	for _, e := range d.Entries {
		if !match(e, a) {
			out = append(out, e)
		}
	}
	d.Entries = append(out, fileEntry{Kind: a.Kind, Instance: a.Instance, Server: a.Server, User: a.User, Label: label, Secret: s.Reveal()})
	return f.save(d)
}

func (f *File) Delete(_ context.Context, a Attributes) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, err := f.load()
	if err != nil {
		return err
	}
	out := d.Entries[:0]
	found := false
	for _, e := range d.Entries {
		if match(e, a) {
			found = true
			continue
		}
		out = append(out, e)
	}
	if !found {
		return nil
	}
	d.Entries = out
	return f.save(d)
}

func (f *File) String() string { return fmt.Sprintf("file(%s)", f.Path) }
