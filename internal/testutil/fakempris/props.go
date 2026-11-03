package fakempris

import (
	"reflect"
	"sort"
	"sync"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
)

// The godbus prop package stores new map values by merging them into the
// old map in place, which keeps stale metadata keys and races with replies
// being encoded. This minimal property store replaces values instead and
// never mutates a value after storing it.

type emitMode int

const (
	emitTrue emitMode = iota
	emitFalse
	emitConst
)

type fprop struct {
	Value    any
	Writable bool
	Emit     emitMode
}

type propStore struct {
	conn *dbus.Conn
	mu   sync.Mutex
	m    map[string]map[string]*fprop
}

var propsIntrospectData = introspect.Interface{
	Name: "org.freedesktop.DBus.Properties",
	Methods: []introspect.Method{
		{Name: "Get", Args: []introspect.Arg{{Name: "interface", Type: "s", Direction: "in"}, {Name: "property", Type: "s", Direction: "in"}, {Name: "value", Type: "v", Direction: "out"}}},
		{Name: "GetAll", Args: []introspect.Arg{{Name: "interface", Type: "s", Direction: "in"}, {Name: "props", Type: "a{sv}", Direction: "out"}}},
		{Name: "Set", Args: []introspect.Arg{{Name: "interface", Type: "s", Direction: "in"}, {Name: "property", Type: "s", Direction: "in"}, {Name: "value", Type: "v", Direction: "in"}}},
	},
	Signals: []introspect.Signal{{Name: "PropertiesChanged", Args: []introspect.Arg{{Name: "interface", Type: "s"}, {Name: "changed_properties", Type: "a{sv}"}, {Name: "invalidates_properties", Type: "as"}}}},
}

func exportProps(conn *dbus.Conn, m map[string]map[string]*fprop) (*propStore, error) {
	s := &propStore{conn: conn, m: m}
	if err := conn.Export(&propsHandler{s}, path, "org.freedesktop.DBus.Properties"); err != nil {
		return nil, err
	}
	return s, nil
}

// Introspection describes an interface's properties.
func (s *propStore) Introspection(iface string) []introspect.Property {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []introspect.Property
	for name, p := range s.m[iface] {
		access := "read"
		if p.Writable {
			access = "readwrite"
		}
		out = append(out, introspect.Property{Name: name, Type: dbus.SignatureOf(p.Value).String(), Access: access})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns a property value.
func (s *propStore) Get(iface, name string) (dbus.Variant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.m[iface][name]
	if !ok {
		return dbus.Variant{}, dbus.NewError("org.freedesktop.DBus.Error.UnknownProperty", []any{name})
	}
	return dbus.MakeVariant(p.Value), nil
}

func (s *propStore) set(iface, name string, v any) {
	s.mu.Lock()
	p, ok := s.m[iface][name]
	if !ok {
		p = &fprop{Emit: emitTrue}
		s.m[iface][name] = p
	}
	p.Value = v
	emit := p.Emit
	s.mu.Unlock()
	if emit == emitTrue {
		defer func() { _ = recover() }()
		_ = s.conn.Emit(path, "org.freedesktop.DBus.Properties.PropertiesChanged",
			iface, map[string]dbus.Variant{name: dbus.MakeVariant(v)}, []string{})
	}
}

type propsHandler struct{ s *propStore }

func (h *propsHandler) Get(iface, name string) (dbus.Variant, *dbus.Error) {
	v, err := h.s.Get(iface, name)
	if err != nil {
		return v, err.(*dbus.Error)
	}
	return v, nil
}

func (h *propsHandler) GetAll(iface string) (map[string]dbus.Variant, *dbus.Error) {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	out := map[string]dbus.Variant{}
	for name, p := range h.s.m[iface] {
		out[name] = dbus.MakeVariant(p.Value)
	}
	return out, nil
}

func (h *propsHandler) Set(iface, name string, v dbus.Variant) *dbus.Error {
	h.s.mu.Lock()
	p, ok := h.s.m[iface][name]
	if !ok {
		h.s.mu.Unlock()
		return dbus.NewError("org.freedesktop.DBus.Error.UnknownProperty", []any{name})
	}
	if !p.Writable {
		h.s.mu.Unlock()
		return dbus.NewError("org.freedesktop.DBus.Error.PropertyReadOnly", []any{name})
	}
	if reflect.TypeOf(v.Value()) != reflect.TypeOf(p.Value) {
		h.s.mu.Unlock()
		return dbus.NewError("org.freedesktop.DBus.Error.InvalidArgs", []any{"wrong type"})
	}
	h.s.mu.Unlock()
	h.s.set(iface, name, v.Value())
	return nil
}
