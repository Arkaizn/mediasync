package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/arkaizn/mediasync/internal/model"
	"github.com/arkaizn/mediasync/internal/provider"
)

// Command routes cmd to the provider owning sessionID. If sessionID is
// empty, the currently selected session is used; the result always names
// the session that was targeted. The command is never redirected to another
// session: if the target is gone, the call fails.
//
// Success contract: the result is returned only after the provider accepted
// the command. Outcome is "verified" if the expected state change was then
// observed within the verification window, otherwise "acknowledged".
func (e *Engine) Command(ctx context.Context, sessionID string, cmd model.Command) (model.CommandResult, error) {
	if err := cmd.Validate(); err != nil {
		return model.CommandResult{}, err
	}
	e.mu.Lock()
	if sessionID == "" {
		sessionID = e.selectedID
		if sessionID == "" {
			e.mu.Unlock()
			return model.CommandResult{}, model.Errorf(model.ErrNoSessionSelected, "no session is selected and no session_id was given")
		}
	}
	before, ok := e.view[sessionID]
	ent := e.sessions[sessionID]
	var prov provider.Provider
	if ok && ent != nil {
		if pe := e.providers[ent.instance]; pe != nil {
			prov = pe.p
		}
	}
	e.mu.Unlock()

	if !ok || prov == nil {
		return model.CommandResult{}, model.Errorf(model.ErrSessionNotFound, "session %q does not exist", sessionID)
	}
	before = before.Clone()
	if before.Status == model.StatusDisconnected {
		return model.CommandResult{}, model.Errorf(model.ErrSessionUnavailable, "session %q is disconnected", sessionID)
	}
	if !before.Controllable {
		return model.CommandResult{}, model.Errorf(model.ErrNotControllable, "session %q can be observed but not controlled", sessionID)
	}
	capab, _ := cmd.RequiredCapability()
	if !before.Has(capab) {
		return model.CommandResult{}, model.Errorf(model.ErrUnsupported, "session %q does not support %q", sessionID, cmd.Name).
			WithData("capabilities", before.Capabilities)
	}

	start := e.opts.Now()
	dctx, cancel := context.WithTimeout(ctx, e.opts.CommandTimeout)
	err := prov.Command(dctx, sessionID, cmd)
	cancel()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && model.CodeOf(err) == model.ErrInternal {
			err = model.Wrap(model.ErrTimeout, err, fmt.Sprintf("provider did not confirm %q within %s", cmd.Name, e.opts.CommandTimeout))
		}
		e.log.Info("command failed", "session", sessionID, "command", cmd.Name, "code", model.CodeOf(err))
		return model.CommandResult{}, err
	}

	// The user acted on this session: it becomes the most recently active.
	e.mu.Lock()
	e.touchLocked(sessionID)
	e.recomputeLocked()
	e.mu.Unlock()

	verify := e.opts.VerifyTimeout
	if before.Origin == model.OriginRemote {
		verify = e.opts.RemoteVerifyTimeout
	}
	pred := expectation(before, cmd, start)
	after, verified := e.waitFor(ctx, sessionID, prov, verify, pred)
	res := model.CommandResult{SessionID: sessionID, Command: cmd.Name, Session: after}
	if verified {
		res.Outcome = model.OutcomeVerified
	} else {
		res.Outcome = model.OutcomeAcknowledged
		res.Detail = fmt.Sprintf("the provider accepted the command but the expected state change was not observed within %s", verify)
	}
	e.log.Debug("command done", "session", sessionID, "command", cmd.Name, "outcome", res.Outcome)
	return res, nil
}

// predicate decides whether the session (nil if removed) reflects the command.
type predicate func(s *model.Session, now time.Time) bool

func expectation(before model.Session, cmd model.Command, issued time.Time) predicate {
	trackKey := func(s *model.Session) string {
		k := ""
		for _, p := range []*string{s.Track.TrackID, s.Track.Title, s.Track.Album} {
			if p != nil {
				k += *p
			}
			k += "\x00"
		}
		for _, a := range s.Track.Artists {
			k += a + ","
		}
		return k
	}
	beforeKey := trackKey(&before)
	beforePos := before.Playback.CurrentPosition(issued)
	switch cmd.Name {
	case model.CmdPlay:
		return func(s *model.Session, _ time.Time) bool { return s != nil && s.Playback.State == model.StatePlaying }
	case model.CmdPause:
		return func(s *model.Session, _ time.Time) bool { return s != nil && s.Playback.State == model.StatePaused }
	case model.CmdPlayPause:
		wasPlaying := before.Playback.State == model.StatePlaying
		return func(s *model.Session, _ time.Time) bool {
			if s == nil {
				return false
			}
			if wasPlaying {
				return s.Playback.State != model.StatePlaying
			}
			return s.Playback.State == model.StatePlaying
		}
	case model.CmdStop:
		// Some providers drop a stopped session entirely (Jellyfin).
		return func(s *model.Session, _ time.Time) bool { return s == nil || s.Playback.State == model.StateStopped }
	case model.CmdNext, model.CmdPrevious:
		return func(s *model.Session, now time.Time) bool {
			if s == nil {
				return false
			}
			if trackKey(s) != beforeKey {
				return true
			}
			// "previous" commonly restarts the current track.
			if cmd.Name == model.CmdPrevious && beforePos != nil && *beforePos > 3000 {
				if p := s.Playback.CurrentPosition(now); p != nil && *p < 3000 {
					return true
				}
			}
			return false
		}
	case model.CmdSeek:
		var target int64
		switch {
		case cmd.PositionMS != nil:
			target = *cmd.PositionMS
		case beforePos != nil:
			target = max(*beforePos+*cmd.OffsetMS, 0)
		default:
			return func(*model.Session, time.Time) bool { return false }
		}
		if before.Track.DurationMS != nil && target > *before.Track.DurationMS {
			target = *before.Track.DurationMS
		}
		return func(s *model.Session, now time.Time) bool {
			if s == nil || s.Playback.PositionAt == nil || !s.Playback.PositionAt.After(issued.Add(-time.Millisecond)) {
				return false
			}
			p := s.Playback.CurrentPosition(now)
			return p != nil && math.Abs(float64(*p-target)) <= 2000
		}
	case model.CmdSetVolume:
		want := *cmd.Volume
		return func(s *model.Session, _ time.Time) bool {
			return s != nil && s.Playback.Volume != nil && math.Abs(*s.Playback.Volume-want) <= 0.02
		}
	}
	return func(*model.Session, time.Time) bool { return false }
}

// waitFor waits until pred holds for the session or the timeout expires.
// Polling providers are asked to refresh at increasing intervals.
func (e *Engine) waitFor(ctx context.Context, id string, prov provider.Provider, timeout time.Duration, pred predicate) (*model.Session, bool) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	refresher, _ := prov.(provider.Refresher)
	schedule := []time.Duration{0, 300 * time.Millisecond, 800 * time.Millisecond, 1500 * time.Millisecond, 2500 * time.Millisecond, 4000 * time.Millisecond}
	var refreshTimer *time.Timer
	var refreshC <-chan time.Time
	start := time.Now()
	armRefresh := func() {
		for refresher != nil && len(schedule) > 0 {
			d := schedule[0] - time.Since(start)
			schedule = schedule[1:]
			if d <= 0 {
				refresher.Refresh()
				continue
			}
			refreshTimer = time.NewTimer(d)
			refreshC = refreshTimer.C
			return
		}
		refreshC = nil
	}
	armRefresh()
	defer func() {
		if refreshTimer != nil {
			refreshTimer.Stop()
		}
	}()
	for {
		e.mu.Lock()
		var cur *model.Session
		if v, ok := e.view[id]; ok {
			c := v.Clone()
			cur = &c
		}
		changed := e.changed
		e.mu.Unlock()
		if pred(cur, e.opts.Now()) {
			return cur, true
		}
		select {
		case <-changed:
		case <-refreshC:
			refresher.Refresh()
			armRefresh()
		case <-deadline.C:
			return cur, false
		case <-ctx.Done():
			return cur, false
		}
	}
}
