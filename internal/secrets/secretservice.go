package secrets

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/arkaizn/mediasync/internal/model"
	"github.com/arkaizn/mediasync/internal/redact"
)

const (
	ssName           = "org.freedesktop.secrets"
	ssPath           = dbus.ObjectPath("/org/freedesktop/secrets")
	ssService        = "org.freedesktop.Secret.Service"
	ssCollection     = "org.freedesktop.Secret.Collection"
	ssItem           = "org.freedesktop.Secret.Item"
	ssPrompt         = "org.freedesktop.Secret.Prompt"
	ssSession        = "org.freedesktop.Secret.Session"
	applicationValue = "mediasync"
)

// secretStruct is the Secret Service (oayays) secret structure.
type secretStruct struct {
	Session     dbus.ObjectPath
	Parameters  []byte
	Value       []byte
	ContentType string
}

// SecretService stores secrets via the freedesktop Secret Service API.
type SecretService struct {
	// Connect returns a session bus connection (injectable for tests).
	Connect func() (*dbus.Conn, error)
	// CallTimeout bounds each D-Bus call.
	CallTimeout time.Duration
	// PromptTimeout bounds how long to wait for an unlock prompt.
	PromptTimeout time.Duration

	mu   sync.Mutex
	conn *dbus.Conn
}

// NewSecretService returns a store using the user's session bus.
func NewSecretService() *SecretService {
	return &SecretService{
		Connect:       func() (*dbus.Conn, error) { return dbus.ConnectSessionBus() },
		CallTimeout:   5 * time.Second,
		PromptTimeout: 2 * time.Minute,
	}
}

func (s *SecretService) Backend() string { return "secret-service" }

func (s *SecretService) bus() (*dbus.Conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil && s.conn.Connected() {
		return s.conn, nil
	}
	c, err := s.Connect()
	if err != nil {
		return nil, model.Wrap(model.ErrCredentialStoreUnavailable, err, "secret-service: cannot connect to the D-Bus session bus")
	}
	s.conn = c
	return c, nil
}

// Close releases the bus connection.
func (s *SecretService) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
	}
}

func (s *SecretService) call(ctx context.Context, obj dbus.BusObject, method string, args ...any) *dbus.Call {
	cctx, cancel := context.WithTimeout(ctx, s.CallTimeout)
	defer cancel()
	return obj.CallWithContext(cctx, method, 0, args...)
}

func unavailable(err error) error {
	var derr dbus.Error
	if errors.As(err, &derr) && (derr.Name == "org.freedesktop.DBus.Error.ServiceUnknown" ||
		derr.Name == "org.freedesktop.DBus.Error.NameHasNoOwner" ||
		derr.Name == "org.freedesktop.DBus.Error.Spawn.ServiceNotFound") {
		return model.Wrap(model.ErrCredentialStoreUnavailable, err,
			"secret-service: no Secret Service provider is running (install/enable GNOME Keyring, KWallet or KeePassXC, or set credentials.store = \"file\")")
	}
	return nil
}

// Check verifies a Secret Service is reachable.
func (s *SecretService) Check(ctx context.Context) error {
	conn, err := s.bus()
	if err != nil {
		return err
	}
	sess, err := s.openSession(ctx, conn)
	if err != nil {
		return err
	}
	s.closeSession(ctx, conn, sess)
	return nil
}

func (s *SecretService) openSession(ctx context.Context, conn *dbus.Conn) (dbus.ObjectPath, error) {
	var out dbus.Variant
	var path dbus.ObjectPath
	err := s.call(ctx, conn.Object(ssName, ssPath), ssService+".OpenSession", "plain", dbus.MakeVariant("")).Store(&out, &path)
	if err != nil {
		if u := unavailable(err); u != nil {
			return "", u
		}
		return "", storeError("secret-service", err, "open a session for")
	}
	return path, nil
}

func (s *SecretService) closeSession(ctx context.Context, conn *dbus.Conn, p dbus.ObjectPath) {
	_ = s.call(ctx, conn.Object(ssName, p), ssSession+".Close").Err
}

func lookupAttrs(a Attributes) map[string]string {
	return map[string]string{
		"application": applicationValue,
		"kind":        a.Kind,
		"instance":    a.Instance,
		"server":      a.Server,
	}
}

// prompt runs a Secret Service prompt and waits for completion.
func (s *SecretService) prompt(ctx context.Context, conn *dbus.Conn, p dbus.ObjectPath) (dbus.Variant, error) {
	if p == "/" || p == "" {
		return dbus.Variant{}, nil
	}
	ch := make(chan *dbus.Signal, 4)
	conn.Signal(ch)
	defer conn.RemoveSignal(ch)
	opts := []dbus.MatchOption{dbus.WithMatchObjectPath(p), dbus.WithMatchInterface(ssPrompt), dbus.WithMatchMember("Completed")}
	if err := conn.AddMatchSignalContext(ctx, opts...); err != nil {
		return dbus.Variant{}, storeError("secret-service", err, "prompt for")
	}
	defer func() { _ = conn.RemoveMatchSignal(opts...) }()
	if err := s.call(ctx, conn.Object(ssName, p), ssPrompt+".Prompt", "").Err; err != nil {
		return dbus.Variant{}, storeError("secret-service", err, "prompt for")
	}
	timeout := time.NewTimer(s.PromptTimeout)
	defer timeout.Stop()
	for {
		select {
		case sig := <-ch:
			if sig.Path != p || sig.Name != ssPrompt+".Completed" || len(sig.Body) < 2 {
				continue
			}
			if dismissed, _ := sig.Body[0].(bool); dismissed {
				return dbus.Variant{}, model.Errorf(model.ErrCredentialStore, "secret-service: the unlock prompt was dismissed")
			}
			v, _ := sig.Body[1].(dbus.Variant)
			return v, nil
		case <-timeout.C:
			return dbus.Variant{}, model.Errorf(model.ErrCredentialStore, "secret-service: timed out waiting for the unlock prompt")
		case <-ctx.Done():
			return dbus.Variant{}, ctx.Err()
		}
	}
}

func (s *SecretService) find(ctx context.Context, conn *dbus.Conn, a Attributes) ([]dbus.ObjectPath, error) {
	var unlocked, locked []dbus.ObjectPath
	if err := s.call(ctx, conn.Object(ssName, ssPath), ssService+".SearchItems", lookupAttrs(a)).Store(&unlocked, &locked); err != nil {
		if u := unavailable(err); u != nil {
			return nil, u
		}
		return nil, storeError("secret-service", err, "search for")
	}
	if len(locked) > 0 {
		var newly []dbus.ObjectPath
		var p dbus.ObjectPath
		if err := s.call(ctx, conn.Object(ssName, ssPath), ssService+".Unlock", locked).Store(&newly, &p); err != nil {
			return nil, storeError("secret-service", err, "unlock")
		}
		if p != "/" {
			if _, err := s.prompt(ctx, conn, p); err != nil {
				return nil, err
			}
			newly = locked
		}
		unlocked = append(unlocked, newly...)
	}
	return unlocked, nil
}

func (s *SecretService) Get(ctx context.Context, a Attributes) (redact.Secret, error) {
	conn, err := s.bus()
	if err != nil {
		return "", err
	}
	items, err := s.find(ctx, conn, a)
	if err != nil {
		return "", err
	}
	if len(items) == 0 {
		return "", ErrNotFound
	}
	sess, err := s.openSession(ctx, conn)
	if err != nil {
		return "", err
	}
	defer s.closeSession(ctx, conn, sess)
	var sec secretStruct
	if err := s.call(ctx, conn.Object(ssName, items[0]), ssItem+".GetSecret", sess).Store(&sec); err != nil {
		return "", storeError("secret-service", err, "read")
	}
	return redact.Secret(sec.Value), nil
}

func (s *SecretService) defaultCollection(ctx context.Context, conn *dbus.Conn) (dbus.ObjectPath, error) {
	var col dbus.ObjectPath
	if err := s.call(ctx, conn.Object(ssName, ssPath), ssService+".ReadAlias", "default").Store(&col); err != nil {
		if u := unavailable(err); u != nil {
			return "", u
		}
		return "", storeError("secret-service", err, "find the default collection for")
	}
	if col == "/" || col == "" {
		return "", model.Errorf(model.ErrCredentialStore, "secret-service: no default keyring/collection exists; create one in your keyring manager")
	}
	return col, nil
}

func (s *SecretService) Set(ctx context.Context, a Attributes, label string, v redact.Secret) error {
	conn, err := s.bus()
	if err != nil {
		return err
	}
	col, err := s.defaultCollection(ctx, conn)
	if err != nil {
		return err
	}
	// Unlock the collection if needed.
	var unlocked []dbus.ObjectPath
	var p dbus.ObjectPath
	if err := s.call(ctx, conn.Object(ssName, ssPath), ssService+".Unlock", []dbus.ObjectPath{col}).Store(&unlocked, &p); err != nil {
		return storeError("secret-service", err, "unlock collection for")
	}
	if _, err := s.prompt(ctx, conn, p); err != nil {
		return err
	}
	sess, err := s.openSession(ctx, conn)
	if err != nil {
		return err
	}
	defer s.closeSession(ctx, conn, sess)
	attrs := lookupAttrs(a)
	if a.User != "" {
		attrs["user"] = a.User
	}
	props := map[string]dbus.Variant{
		"org.freedesktop.Secret.Item.Label":      dbus.MakeVariant(label),
		"org.freedesktop.Secret.Item.Attributes": dbus.MakeVariant(attrs),
	}
	sec := secretStruct{Session: sess, Parameters: []byte{}, Value: []byte(v.Reveal()), ContentType: "text/plain"}
	var item dbus.ObjectPath
	if err := s.call(ctx, conn.Object(ssName, col), ssCollection+".CreateItem", props, sec, true).Store(&item, &p); err != nil {
		return storeError("secret-service", err, "store")
	}
	if _, err := s.prompt(ctx, conn, p); err != nil {
		return err
	}
	return nil
}

func (s *SecretService) Delete(ctx context.Context, a Attributes) error {
	conn, err := s.bus()
	if err != nil {
		return err
	}
	items, err := s.find(ctx, conn, a)
	if err != nil {
		return err
	}
	for _, it := range items {
		var p dbus.ObjectPath
		if err := s.call(ctx, conn.Object(ssName, it), ssItem+".Delete").Store(&p); err != nil {
			return storeError("secret-service", err, "delete")
		}
		if _, err := s.prompt(ctx, conn, p); err != nil {
			return err
		}
	}
	return nil
}

func (s *SecretService) String() string { return fmt.Sprintf("secret-service(%s)", ssName) }
