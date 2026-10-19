// Package fakesecrets implements a minimal in-process freedesktop Secret
// Service on a D-Bus connection for tests.
package fakesecrets

import (
	"fmt"
	"sync"

	"github.com/godbus/dbus/v5"
)

const (
	root    = dbus.ObjectPath("/org/freedesktop/secrets")
	colPath = dbus.ObjectPath("/org/freedesktop/secrets/collection/login")
)

type secret struct {
	Session     dbus.ObjectPath
	Parameters  []byte
	Value       []byte
	ContentType string
}

type item struct {
	path   dbus.ObjectPath
	attrs  map[string]string
	label  string
	value  []byte
	locked bool
}

// Service is a fake Secret Service.
type Service struct {
	conn *dbus.Conn

	mu             sync.Mutex
	items          map[dbus.ObjectPath]*item
	n              int
	dismissPrompts bool
	noDefault      bool
	prompts        int
}

// SetDismissPrompts makes every prompt report dismissal.
func (s *Service) SetDismissPrompts(v bool) { s.mu.Lock(); s.dismissPrompts = v; s.mu.Unlock() }

// SetNoDefault makes ReadAlias("default") return "/".
func (s *Service) SetNoDefault(v bool) { s.mu.Lock(); s.noDefault = v; s.mu.Unlock() }

// Prompts returns how many prompts were shown.
func (s *Service) Prompts() int { s.mu.Lock(); defer s.mu.Unlock(); return s.prompts }

// Start exports the service on conn and claims org.freedesktop.secrets.
func Start(conn *dbus.Conn) (*Service, error) {
	s := &Service{conn: conn, items: map[dbus.ObjectPath]*item{}}
	if err := conn.Export(&serviceHandler{s}, root, "org.freedesktop.Secret.Service"); err != nil {
		return nil, err
	}
	if err := conn.Export(&collectionHandler{s}, colPath, "org.freedesktop.Secret.Collection"); err != nil {
		return nil, err
	}
	reply, err := conn.RequestName("org.freedesktop.secrets", dbus.NameFlagDoNotQueue)
	if err != nil {
		return nil, err
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		return nil, fmt.Errorf("secret service name taken")
	}
	return s, nil
}

// Items returns stored items as attribute maps with their values.
func (s *Service) Items() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for _, it := range s.items {
		out[it.attrs["instance"]+"|"+it.attrs["server"]] = string(it.value)
	}
	return out
}

// LockAll locks every item.
func (s *Service) LockAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, it := range s.items {
		it.locked = true
	}
}

func (s *Service) newPrompt(onComplete func()) dbus.ObjectPath {
	s.mu.Lock()
	s.n++
	p := dbus.ObjectPath(fmt.Sprintf("/org/freedesktop/secrets/prompt/p%d", s.n))
	s.mu.Unlock()
	_ = s.conn.Export(&promptHandler{s: s, path: p, done: onComplete}, p, "org.freedesktop.Secret.Prompt")
	return p
}

type serviceHandler struct{ s *Service }

func (h *serviceHandler) OpenSession(alg string, in dbus.Variant) (dbus.Variant, dbus.ObjectPath, *dbus.Error) {
	if alg != "plain" {
		return dbus.Variant{}, "", dbus.NewError("org.freedesktop.DBus.Error.NotSupported", []any{"algorithm"})
	}
	h.s.mu.Lock()
	h.s.n++
	p := dbus.ObjectPath(fmt.Sprintf("/org/freedesktop/secrets/session/s%d", h.s.n))
	h.s.mu.Unlock()
	_ = h.s.conn.Export(&sessionHandler{h.s, p}, p, "org.freedesktop.Secret.Session")
	return dbus.MakeVariant(""), p, nil
}

func (h *serviceHandler) SearchItems(attrs map[string]string) ([]dbus.ObjectPath, []dbus.ObjectPath, *dbus.Error) {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	var unlocked, locked []dbus.ObjectPath
	for _, it := range h.s.items {
		ok := true
		for k, v := range attrs {
			if it.attrs[k] != v {
				ok = false
			}
		}
		if !ok {
			continue
		}
		if it.locked {
			locked = append(locked, it.path)
		} else {
			unlocked = append(unlocked, it.path)
		}
	}
	return unlocked, locked, nil
}

func (h *serviceHandler) Unlock(objs []dbus.ObjectPath) ([]dbus.ObjectPath, dbus.ObjectPath, *dbus.Error) {
	h.s.mu.Lock()
	var need []dbus.ObjectPath
	var already []dbus.ObjectPath
	for _, o := range objs {
		if it, ok := h.s.items[o]; ok && it.locked {
			need = append(need, o)
		} else {
			already = append(already, o)
		}
	}
	h.s.mu.Unlock()
	if len(need) == 0 {
		return already, "/", nil
	}
	p := h.s.newPrompt(func() {
		h.s.mu.Lock()
		for _, o := range need {
			if it, ok := h.s.items[o]; ok {
				it.locked = false
			}
		}
		h.s.mu.Unlock()
	})
	return already, p, nil
}

func (h *serviceHandler) ReadAlias(name string) (dbus.ObjectPath, *dbus.Error) {
	h.s.mu.Lock()
	noDefault := h.s.noDefault
	h.s.mu.Unlock()
	if name == "default" && !noDefault {
		return colPath, nil
	}
	return "/", nil
}

type collectionHandler struct{ s *Service }

func (h *collectionHandler) CreateItem(props map[string]dbus.Variant, sec secret, replace bool) (dbus.ObjectPath, dbus.ObjectPath, *dbus.Error) {
	attrs, _ := props["org.freedesktop.Secret.Item.Attributes"].Value().(map[string]string)
	label, _ := props["org.freedesktop.Secret.Item.Label"].Value().(string)
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	if replace {
		for p, it := range h.s.items {
			same := len(it.attrs) == len(attrs)
			for k, v := range attrs {
				if it.attrs[k] != v {
					same = false
				}
			}
			if same {
				it.value = sec.Value
				it.label = label
				return p, "/", nil
			}
		}
	}
	h.s.n++
	p := dbus.ObjectPath(fmt.Sprintf("%s/i%d", colPath, h.s.n))
	it := &item{path: p, attrs: attrs, label: label, value: sec.Value}
	h.s.items[p] = it
	_ = h.s.conn.Export(&itemHandler{h.s, p}, p, "org.freedesktop.Secret.Item")
	return p, "/", nil
}

type itemHandler struct {
	s    *Service
	path dbus.ObjectPath
}

func (h *itemHandler) GetSecret(session dbus.ObjectPath) (secret, *dbus.Error) {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	it, ok := h.s.items[h.path]
	if !ok {
		return secret{}, dbus.NewError("org.freedesktop.Secret.Error.NoSuchObject", nil)
	}
	if it.locked {
		return secret{}, dbus.NewError("org.freedesktop.Secret.Error.IsLocked", nil)
	}
	return secret{Session: session, Parameters: []byte{}, Value: it.value, ContentType: "text/plain"}, nil
}

func (h *itemHandler) Delete() (dbus.ObjectPath, *dbus.Error) {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	delete(h.s.items, h.path)
	return "/", nil
}

type sessionHandler struct {
	s    *Service
	path dbus.ObjectPath
}

func (h *sessionHandler) Close() *dbus.Error { return nil }

type promptHandler struct {
	s    *Service
	path dbus.ObjectPath
	done func()
}

func (h *promptHandler) Prompt(window string) *dbus.Error {
	h.s.mu.Lock()
	h.s.prompts++
	dismiss := h.s.dismissPrompts
	h.s.mu.Unlock()
	go func() {
		if !dismiss && h.done != nil {
			h.done()
		}
		_ = h.s.conn.Emit(h.path, "org.freedesktop.Secret.Prompt.Completed", dismiss, dbus.MakeVariant([]dbus.ObjectPath{}))
	}()
	return nil
}
