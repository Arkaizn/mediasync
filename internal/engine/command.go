package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
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

	// Queue commands for a local player whose queue comes from its linked
	// remote session (e.g. Feishin and its own Jellyfin session) are carried
	// out through that session. Nothing else is ever redirected.
	target, targetProv, targetBefore := sessionID, prov, before
	if cmd.Name == model.CmdQueueJump || cmd.Name == model.CmdPlayItems {
		if rid, rp, rv, ok := e.viaLinked(sessionID, capab); ok {
			target, targetProv, targetBefore = rid, rp, rv
		}
	}
	var pred predicate
	if cmd.Name == model.CmdQueueJump {
		qs, ok := targetProv.(provider.QueueSource)
		if !ok {
			return model.CommandResult{}, model.Errorf(model.ErrUnsupported, "session %q has no queue", sessionID)
		}
		q, err := qs.Queue(ctx, target)
		if err != nil {
			return model.CommandResult{}, err
		}
		if !q.Available {
			return model.CommandResult{}, model.Errorf(model.ErrUnsupported, "the queue of session %q is not available", sessionID)
		}
		if *cmd.Index >= len(q.Items) {
			return model.CommandResult{}, model.Errorf(model.ErrInvalidArgument, "index %d is outside the queue (%d items)", *cmd.Index, len(q.Items))
		}
		want := q.Items[*cmd.Index].ItemID
		pred = func(s *model.Session, _ time.Time) bool {
			return s != nil && s.Track.TrackID != nil && strings.EqualFold(*s.Track.TrackID, want)
		}
	}

	start := e.opts.Now()
	if pred == nil {
		pred = expectation(targetBefore, cmd, start)
	}
	dctx, cancel := context.WithTimeout(ctx, e.opts.CommandTimeout)
	err := targetProv.Command(dctx, target, cmd)
	cancel()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && model.CodeOf(err) == model.ErrInternal {
			err = model.Wrap(model.ErrTimeout, err, fmt.Sprintf("provider did not confirm %q within %s", cmd.Name, e.opts.CommandTimeout))
		}
		attrs := []any{"session", sessionID, "command", cmd.Name, "code", model.CodeOf(err)}
		if st, ok := model.AsError(err).Data["http_status"]; ok {
			attrs = append(attrs, "http_status", st)
		}
		// Error messages never contain secrets (providers only name paths).
		e.log.Info("command failed", append(attrs, "err", err.Error())...)
		return model.CommandResult{}, err
	}

	// The user acted on this session: it becomes the most recently active.
	e.mu.Lock()
	e.touchLocked(sessionID)
	if target != sessionID {
		e.touchLocked(target)
	}
	e.recomputeLocked()
	e.mu.Unlock()

	verify := e.opts.VerifyTimeout
	if targetBefore.Origin == model.OriginRemote {
		verify = e.opts.RemoteVerifyTimeout
	}
	after, verified := e.waitFor(ctx, target, targetProv, verify, pred)
	res := model.CommandResult{SessionID: sessionID, Command: cmd.Name, Session: after}
	if target != sessionID {
		res.ViaSessionID = target
		if s, ok := e.Session(sessionID); ok {
			res.Session = &s
		}
	}
	if verified {
		res.Outcome = model.OutcomeVerified
	} else {
		res.Outcome = model.OutcomeAcknowledged
		res.Detail = fmt.Sprintf("the provider accepted the command but the expected state change was not observed within %s", verify)
		if startsTrack(cmd) && after != nil && sameTrack(&targetBefore, after) {
			res.Detail = fmt.Sprintf("the provider accepted the command, but the player has not reported a new track within %s: "+
				"the session still shows the previous one (some clients report a remotely started track late or not at all)", verify)
		}
	}
	e.log.Debug("command done", "session", sessionID, "command", cmd.Name, "outcome", res.Outcome)
	return res, nil
}

// startsTrack reports whether cmd is expected to start another track.
func startsTrack(cmd model.Command) bool {
	return cmd.Name == model.CmdQueueJump || cmd.Name == model.CmdNext || cmd.Name == model.CmdPrevious ||
		(cmd.Name == model.CmdPlayItems && cmd.PlayMode() == model.PlayModeNow)
}

// sameTrack reports whether both sessions show the same track.
func sameTrack(a, b *model.Session) bool {
	eq := func(x, y *string) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return eq(a.Track.TrackID, b.Track.TrackID) && eq(a.Track.Title, b.Track.Title)
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
	case model.CmdPlayItems:
		if cmd.PlayMode() != model.PlayModeNow {
			// Adding to the queue is only observable through the queue.
			beforeRev := before.QueueRevision
			return func(s *model.Session, _ time.Time) bool {
				return s != nil && s.QueueRevision != beforeRev
			}
		}
		beforeRev := before.QueueRevision
		return func(s *model.Session, now time.Time) bool {
			if s == nil {
				return false
			}
			if trackKey(s) != beforeKey {
				return true
			}
			// The queue was replaced (e.g. the first new item is the one
			// already playing). A position reset on the same track is not
			// taken as proof: clients that report a remote-started track
			// late (Finamp) report the previous track at 0:00 instead.
			return s.QueueRevision != beforeRev
		}
	case model.CmdSetShuffle:
		want := *cmd.Shuffle
		return func(s *model.Session, _ time.Time) bool {
			return s != nil && s.Playback.Shuffle != nil && *s.Playback.Shuffle == want
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
