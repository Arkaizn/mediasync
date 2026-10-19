// Package engine aggregates sessions from all providers, applies
// presentation settings (mode, filters, deduplication), selects a session
// deterministically, routes commands to the owning provider and publishes
// change events.
package engine

import (
	"context"
	"log/slog"
	"math"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/arkaizn/mediasync/internal/model"
	"github.com/arkaizn/mediasync/internal/provider"
)

// Options configure an Engine.
type Options struct {
	Settings Settings
	Host     HostInfo
	Logger   *slog.Logger
	// CommandTimeout bounds provider command dispatch.
	CommandTimeout time.Duration
	// VerifyTimeout bounds how long the engine waits to observe a command's
	// effect for local providers. RemoteVerifyTimeout applies to remote ones.
	VerifyTimeout       time.Duration
	RemoteVerifyTimeout time.Duration
	// SelectionGrace keeps an explicit selection for a session that vanished
	// (e.g. a restarting player) for this long.
	SelectionGrace time.Duration
	// SubscriberBuffer is the per-subscriber event queue size.
	SubscriberBuffer int
	// Now is the clock (tests).
	Now func() time.Time
}

func (o *Options) defaults() {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.CommandTimeout <= 0 {
		o.CommandTimeout = 5 * time.Second
	}
	if o.VerifyTimeout <= 0 {
		o.VerifyTimeout = 1500 * time.Millisecond
	}
	if o.RemoteVerifyTimeout <= 0 {
		o.RemoteVerifyTimeout = 5 * time.Second
	}
	if o.SelectionGrace <= 0 {
		o.SelectionGrace = 30 * time.Second
	}
	if o.SubscriberBuffer <= 0 {
		o.SubscriberBuffer = 256
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

type entry struct {
	raw      model.Session // as last received from the provider (+UpdatedAt)
	instance string
	// activeAt is when the session last started playing (or was first seen);
	// used for tie-breaking.
	activeAt time.Time
	seq      uint64 // insertion order, final deterministic tie-breaker after ID
}

type providerEntry struct {
	p      provider.Provider
	status model.ProviderStatus
	cancel context.CancelFunc
	done   chan struct{}
	ids    map[string]bool
}

// Engine is the central session aggregator.
type Engine struct {
	opts Options
	log  *slog.Logger

	mu        sync.Mutex
	settings  Settings
	sessions  map[string]*entry
	providers map[string]*providerEntry
	seq       uint64

	explicit     string    // explicitly selected session ID ("" = auto)
	explicitLost time.Time // when the explicit session became unavailable

	// view is the last computed presentation state.
	view       map[string]model.Session // all sessions with computed fields
	selectedID string
	selReason  string
	dupReasons map[string]DuplicateReason

	revision uint64
	subs     map[*Subscription]struct{}
	// changed is closed and replaced whenever the store changes (for waiters).
	changed chan struct{}

	started time.Time
}

// New creates an engine.
func New(opts Options) *Engine {
	opts.defaults()
	if opts.Settings.Mode == "" {
		opts.Settings = DefaultSettings()
	}
	e := &Engine{
		opts:      opts,
		log:       opts.Logger.With("component", "engine"),
		settings:  opts.Settings,
		sessions:  map[string]*entry{},
		providers: map[string]*providerEntry{},
		view:      map[string]model.Session{},
		subs:      map[*Subscription]struct{}{},
		changed:   make(chan struct{}),
		started:   opts.Now(),
	}
	return e
}

// ---------------------------------------------------------------------------
// Provider supervision

// AddProvider starts a provider under supervision. If it exits with an
// error it is restarted with exponential backoff.
func (e *Engine) AddProvider(p provider.Provider) {
	ctx, cancel := context.WithCancel(context.Background())
	pe := &providerEntry{
		p: p, cancel: cancel, done: make(chan struct{}), ids: map[string]bool{},
		status: model.ProviderStatus{Instance: p.Instance(), Kind: p.Kind(), Origin: p.Origin(), State: model.ProviderStarting},
	}
	e.mu.Lock()
	if old, ok := e.providers[p.Instance()]; ok {
		e.mu.Unlock()
		e.RemoveProvider(old.p.Instance())
		e.mu.Lock()
	}
	e.providers[p.Instance()] = pe
	e.emitLocked(Event{Type: EventProviderStatus, Provider: model.Ptr(pe.status)})
	e.mu.Unlock()
	go e.supervise(ctx, pe)
}

// RemoveProvider stops a provider and removes its sessions.
func (e *Engine) RemoveProvider(instance string) {
	e.mu.Lock()
	pe, ok := e.providers[instance]
	e.mu.Unlock()
	if !ok {
		return
	}
	pe.cancel()
	select {
	case <-pe.done:
	case <-time.After(10 * time.Second):
		e.log.Warn("provider did not stop in time", "provider", instance)
	}
	e.mu.Lock()
	if e.providers[instance] == pe {
		delete(e.providers, instance)
	}
	e.removeProviderSessionsLocked(pe)
	e.emitLocked(Event{Type: EventProviderRemoved, ProviderInstance: instance})
	e.recomputeLocked()
	e.mu.Unlock()
}

// Close stops all providers.
func (e *Engine) Close() {
	e.mu.Lock()
	var ids []string
	for id := range e.providers {
		ids = append(ids, id)
	}
	e.mu.Unlock()
	for _, id := range ids {
		e.RemoveProvider(id)
	}
	e.mu.Lock()
	for s := range e.subs {
		s.closeLocked(CloseShutdown)
	}
	e.mu.Unlock()
}

func (e *Engine) supervise(ctx context.Context, pe *providerEntry) {
	defer close(pe.done)
	backoff := time.Second
	const maxBackoff = time.Minute
	sink := &providerSink{e: e, pe: pe}
	for {
		startedAt := e.opts.Now()
		err := pe.p.Run(ctx, sink)
		// Whatever happened, the provider's sessions are no longer live.
		e.mu.Lock()
		e.removeProviderSessionsLocked(pe)
		e.recomputeLocked()
		e.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		if e.opts.Now().Sub(startedAt) > maxBackoff {
			backoff = time.Second
		}
		e.log.Warn("provider stopped; restarting", "provider", pe.p.Instance(), "err", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func (e *Engine) removeProviderSessionsLocked(pe *providerEntry) {
	for id := range pe.ids {
		delete(e.sessions, id)
	}
	pe.ids = map[string]bool{}
}

type providerSink struct {
	e  *Engine
	pe *providerEntry
}

func (s *providerSink) Upsert(sess model.Session) { s.e.upsert(s.pe, sess) }
func (s *providerSink) Remove(id string)          { s.e.remove(s.pe, id) }
func (s *providerSink) SetStatus(st model.ProviderStatus) {
	s.e.setStatus(s.pe, st)
}

func (e *Engine) upsert(pe *providerEntry, s model.Session) {
	now := e.opts.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.providers[pe.p.Instance()] != pe {
		return // provider was removed
	}
	if s.ID == "" {
		e.log.Error("provider published session without ID", "provider", pe.p.Instance())
		return
	}
	old, exists := e.sessions[s.ID]
	if exists && old.instance != pe.p.Instance() {
		// A provider may never overwrite another provider's session (e.g. a
		// remote provider replacing a local player's data).
		e.log.Error("provider tried to overwrite a session it does not own",
			"provider", pe.p.Instance(), "session", s.ID, "owner", old.instance)
		return
	}
	s.ProviderInstance = pe.p.Instance()
	s.Origin = pe.p.Origin()
	s.Capabilities = model.SortCapabilities(s.Capabilities)
	if !s.Controllable {
		s.Capabilities = []model.Capability{}
	}
	if s.Playback.State == "" {
		s.Playback.State = model.StateUnknown
	}
	if s.Status == "" {
		s.Status = model.StatusConnected
	}
	if s.LastSeen.IsZero() {
		s.LastSeen = now
	}
	if !exists {
		e.seq++
		s.UpdatedAt = now
		e.sessions[s.ID] = &entry{raw: s, instance: pe.p.Instance(), activeAt: now, seq: e.seq}
		pe.ids[s.ID] = true
		e.recomputeLocked()
		return
	}
	if !meaningfulChange(&old.raw, &s) {
		// Keep the fresher position sample and last-seen time silently.
		old.raw.Playback.PositionMS = s.Playback.PositionMS
		old.raw.Playback.PositionAt = s.Playback.PositionAt
		old.raw.LastSeen = s.LastSeen
		e.syncViewSilentLocked(s.ID, old.raw)
		return
	}
	if s.Playback.State == model.StatePlaying && old.raw.Playback.State != model.StatePlaying {
		old.activeAt = now
	}
	s.UpdatedAt = now
	old.raw = s
	e.recomputeLocked()
}

// syncViewSilentLocked refreshes the stored view copy without emitting events.
func (e *Engine) syncViewSilentLocked(id string, raw model.Session) {
	if v, ok := e.view[id]; ok {
		v.Playback.PositionMS = raw.Playback.PositionMS
		v.Playback.PositionAt = raw.Playback.PositionAt
		v.LastSeen = raw.LastSeen
		e.view[id] = v
	}
	e.signalChangedLocked()
}

func (e *Engine) remove(pe *providerEntry, id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ent, ok := e.sessions[id]
	if !ok || ent.instance != pe.p.Instance() {
		return
	}
	delete(e.sessions, id)
	delete(pe.ids, id)
	e.recomputeLocked()
}

func (e *Engine) setStatus(pe *providerEntry, st model.ProviderStatus) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.providers[pe.p.Instance()] != pe {
		return
	}
	st.Instance, st.Kind, st.Origin = pe.p.Instance(), pe.p.Kind(), pe.p.Origin()
	if st.LastSuccess == nil {
		st.LastSuccess = pe.status.LastSuccess
	}
	if st.LastError == nil && st.State != model.ProviderRunning {
		st.LastError = pe.status.LastError
	}
	st.Sessions = len(pe.ids)
	if statusEqual(pe.status, st) {
		pe.status = st
		return
	}
	pe.status = st
	e.emitLocked(Event{Type: EventProviderStatus, Provider: model.Ptr(st)})
}

func statusEqual(a, b model.ProviderStatus) bool {
	return a.State == b.State && a.Message == b.Message && a.Sessions == b.Sessions &&
		((a.LastError == nil) == (b.LastError == nil)) &&
		(a.LastError == nil || (a.LastError.Code == b.LastError.Code && a.LastError.Message == b.LastError.Message))
}

// meaningfulChange reports whether b differs from a in a way consumers care
// about. Position changes that agree with extrapolation (±1.5 s) and
// LastSeen changes are not meaningful.
func meaningfulChange(a, b *model.Session) bool {
	x, y := a.Clone(), b.Clone()
	ap, bp := x.Playback, y.Playback
	x.LastSeen, y.LastSeen = time.Time{}, time.Time{}
	x.UpdatedAt, y.UpdatedAt = time.Time{}, time.Time{}
	x.Playback.PositionMS, y.Playback.PositionMS = nil, nil
	x.Playback.PositionAt, y.Playback.PositionAt = nil, nil
	if !sessionsEqual(&x, &y) {
		return true
	}
	if (ap.PositionMS == nil) != (bp.PositionMS == nil) {
		return true
	}
	if ap.PositionMS == nil {
		return false
	}
	at := time.Now()
	if bp.PositionAt != nil {
		at = *bp.PositionAt
	}
	extrap := ap.CurrentPosition(at)
	return math.Abs(float64(*extrap-*bp.PositionMS)) > 1500
}

// ---------------------------------------------------------------------------
// View computation

// recomputeLocked rebuilds the presentation view and emits diff events.
func (e *Engine) recomputeLocked() {
	now := e.opts.Now()
	all := make([]*model.Session, 0, len(e.sessions))
	for _, ent := range e.sessions {
		s := ent.raw.Clone()
		all = append(all, &s)
	}
	dup := applyDedup(e.settings, e.opts.Host, all)
	for _, s := range all {
		s.Kind = s.ComputeKind()
		s.Hidden = string(e.settings.hiddenReason(s))
	}

	newView := make(map[string]model.Session, len(all))
	for _, s := range all {
		newView[s.ID] = *s
	}

	// Selection.
	visible := e.rankedLocked(all, false)
	selID, reason := "", ""
	if e.explicit != "" {
		if v, ok := newView[e.explicit]; ok && v.Hidden == "" {
			selID, reason = e.explicit, "user"
			e.explicitLost = time.Time{}
		} else {
			if e.explicitLost.IsZero() {
				e.explicitLost = now
			}
			if now.Sub(e.explicitLost) > e.opts.SelectionGrace {
				e.log.Info("explicitly selected session gone; reverting to automatic selection", "session", e.explicit)
				e.explicit = ""
				e.explicitLost = time.Time{}
			}
		}
	}
	if selID == "" && len(visible) > 0 {
		selID, reason = visible[0].ID, "auto"
	}

	// Diff and emit.
	for id, old := range e.view {
		if _, ok := newView[id]; !ok {
			e.emitLocked(Event{Type: EventSessionRemoved, SessionID: id, Session: model.Ptr(old)})
		}
	}
	ids := make([]string, 0, len(newView))
	for id := range newView {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		nv := newView[id]
		old, existed := e.view[id]
		switch {
		case !existed:
			e.emitLocked(Event{Type: EventSessionAdded, SessionID: id, Session: model.Ptr(nv)})
		case !sessionsEqual(&old, &nv):
			e.emitLocked(Event{Type: EventSessionUpdated, SessionID: id, Session: model.Ptr(nv)})
		}
	}
	e.view = newView
	e.dupReasons = dup
	if selID != e.selectedID || reason != e.selReason {
		e.selectedID, e.selReason = selID, reason
		ev := Event{Type: EventSelectionChanged, Reason: reason}
		if selID != "" {
			ev.SessionID = selID
			ev.Session = model.Ptr(newView[selID])
		}
		e.emitLocked(ev)
	}
	e.signalChangedLocked()
}

func (e *Engine) signalChangedLocked() {
	close(e.changed)
	e.changed = make(chan struct{})
}

func stateRank(s model.PlaybackState) int {
	switch s {
	case model.StatePlaying:
		return 0
	case model.StatePaused:
		return 1
	}
	return 2
}

func statusRank(s model.ConnectionStatus) int {
	switch s {
	case model.StatusConnected:
		return 0
	case model.StatusStale:
		return 1
	}
	return 2
}

// rankedLocked returns sessions in deterministic presentation order. When
// includeHidden is false, hidden sessions are dropped.
//
// Order:
//  1. playing before paused before stopped/unknown
//  2. the preferred player first
//  3. the preferred origin (local by default) first
//  4. connected before stale before disconnected
//  5. controllable before view-only
//  6. most recently started playing (or appeared) first
//  7. session ID ascending
func (e *Engine) rankedLocked(all []*model.Session, includeHidden bool) []*model.Session {
	out := make([]*model.Session, 0, len(all))
	for _, s := range all {
		if includeHidden || s.Hidden == "" {
			out = append(out, s)
		}
	}
	pref := e.settings.PreferredPlayer
	originRank := func(o model.Origin) int {
		switch e.settings.PreferOrigin {
		case PreferLocal:
			if o == model.OriginLocal {
				return 0
			}
			return 1
		case PreferRemote:
			if o == model.OriginRemote {
				return 0
			}
			return 1
		}
		return 0
	}
	b2i := func(b bool) int {
		if b {
			return 0
		}
		return 1
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if x, y := stateRank(a.Playback.State), stateRank(b.Playback.State); x != y {
			return x < y
		}
		if pref != "" {
			if x, y := b2i(a.Player.Matches(pref)), b2i(b.Player.Matches(pref)); x != y {
				return x < y
			}
		}
		if x, y := originRank(a.Origin), originRank(b.Origin); x != y {
			return x < y
		}
		if x, y := statusRank(a.Status), statusRank(b.Status); x != y {
			return x < y
		}
		if x, y := b2i(a.Controllable), b2i(b.Controllable); x != y {
			return x < y
		}
		ea, eb := e.sessions[a.ID], e.sessions[b.ID]
		if ea != nil && eb != nil && !ea.activeAt.Equal(eb.activeAt) {
			return ea.activeAt.After(eb.activeAt)
		}
		return a.ID < b.ID
	})
	return out
}

// ---------------------------------------------------------------------------
// Queries

// Snapshot is a consistent view of the engine state.
type Snapshot struct {
	Revision   uint64          `json:"revision"`
	Sessions   []model.Session `json:"sessions"`
	SelectedID *string         `json:"selected_id"`
	Reason     string          `json:"selection_reason,omitempty"`
	Settings   Settings        `json:"settings"`
}

// Sessions lists sessions in presentation order.
func (e *Engine) Sessions(f Filter) Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.snapshotLocked(f)
}

func (e *Engine) snapshotLocked(f Filter) Snapshot {
	all := make([]*model.Session, 0, len(e.view))
	for _, v := range e.view {
		v := v.Clone()
		all = append(all, &v)
	}
	ranked := e.rankedLocked(all, f.IncludeHidden)
	snap := Snapshot{Revision: e.revision, Sessions: []model.Session{}, Settings: e.settingsCopyLocked()}
	for _, s := range ranked {
		if f.match(s) {
			snap.Sessions = append(snap.Sessions, *s)
		}
	}
	if e.selectedID != "" {
		snap.SelectedID = model.Ptr(e.selectedID)
		snap.Reason = e.selReason
	}
	return snap
}

func (e *Engine) settingsCopyLocked() Settings {
	s := e.settings
	s.DedupLinks = slices.Clone(e.settings.DedupLinks)
	return s
}

// Session returns one session (hidden ones included).
func (e *Engine) Session(id string) (model.Session, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	v, ok := e.view[id]
	return v.Clone(), ok
}

// Selected returns the selected session and the reason ("user" or "auto").
func (e *Engine) Selected() (*model.Session, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.selectedID == "" {
		return nil, ""
	}
	v := e.view[e.selectedID].Clone()
	return &v, e.selReason
}

// Select sets the explicit selection. An empty id returns to automatic
// selection.
func (e *Engine) Select(id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if id != "" {
		v, ok := e.view[id]
		if !ok {
			return model.Errorf(model.ErrSessionNotFound, "session %q does not exist", id)
		}
		if v.Hidden != "" {
			return model.Errorf(model.ErrInvalidArgument, "session %q is hidden (%s) and cannot be selected", id, v.Hidden)
		}
	}
	e.explicit = id
	e.explicitLost = time.Time{}
	e.recomputeLocked()
	return nil
}

// Settings returns the current settings.
func (e *Engine) Settings() Settings {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.settingsCopyLocked()
}

// SetSettings replaces the presentation settings.
func (e *Engine) SetSettings(s Settings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.settings = s
	e.settings.DedupLinks = slices.Clone(s.DedupLinks)
	e.emitLocked(Event{Type: EventSettingsChanged, Settings: model.Ptr(e.settingsCopyLocked())})
	e.recomputeLocked()
	return nil
}

// ProviderStatuses returns the status of all providers, sorted by instance.
func (e *Engine) ProviderStatuses() []model.ProviderStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]model.ProviderStatus, 0, len(e.providers))
	for _, pe := range e.providers {
		st := pe.status
		st.Sessions = len(pe.ids)
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Instance < out[j].Instance })
	return out
}

// Provider returns a running provider by instance name.
func (e *Engine) Provider(instance string) (provider.Provider, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	pe, ok := e.providers[instance]
	if !ok {
		return nil, false
	}
	return pe.p, true
}

// Diagnostics returns engine-level diagnostic data.
func (e *Engine) Diagnostics() map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	dups := map[string]string{}
	for id, r := range e.dupReasons {
		dups[id] = string(r)
	}
	return map[string]any{
		"revision":           e.revision,
		"sessions_total":     len(e.view),
		"subscribers":        len(e.subs),
		"explicit_selection": e.explicit,
		"selected":           e.selectedID,
		"selection_reason":   e.selReason,
		"duplicates":         dups,
		"uptime_seconds":     int64(e.opts.Now().Sub(e.started).Seconds()),
		"hostname":           e.opts.Host.Hostname,
	}
}

// Tick re-evaluates time-dependent state (selection grace). The daemon
// calls it periodically.
func (e *Engine) Tick() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.explicit != "" && !e.explicitLost.IsZero() {
		e.recomputeLocked()
	}
}
