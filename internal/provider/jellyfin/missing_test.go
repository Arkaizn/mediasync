package jellyfin

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/arkaizn/mediasync/internal/model"
	"github.com/arkaizn/mediasync/internal/provider/providertest"
	"github.com/arkaizn/mediasync/internal/testutil/fakejellyfin"
)

func graceEnv(t *testing.T, grace time.Duration, logs *syncBuf) *env {
	e := newEnv(t, func(c *Config) { c.RemoveMissingAfter = grace })
	if logs != nil {
		p, err := New(e.prov.cfg, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
		if err != nil {
			t.Fatal(err)
		}
		e.prov = p
	}
	return e
}

func TestMissingSessionGraceKeepsSessionAndContext(t *testing.T) {
	logs := &syncBuf{}
	e := graceEnv(t, 400*time.Millisecond, logs)
	library(e.srv)
	ph := queuePhone(e.srv)
	e.login(t)
	e.run(t)
	s := e.sessionFor(t, "iphone-q", "Finamp", nil)
	ctx := context.Background()
	if err := e.prov.Command(ctx, s.ID, model.Command{Name: model.CmdPlayItems, ItemIDs: []string{pl}}); err != nil {
		t.Fatal(err)
	}
	e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool { return x.Has(model.CapQueue) && *x.Track.TrackID == t3 })

	// Missing for about two polls (poll interval 50 ms), then back.
	e.srv.Update(ph.SessionID, func(c *fakejellyfin.Client) { c.Hidden = true })
	time.Sleep(150 * time.Millisecond)
	// A command meanwhile is still sent (the server still accepts it).
	if err := e.prov.Command(ctx, s.ID, model.Command{Name: model.CmdPause}); err != nil {
		t.Errorf("command during the blip: %v", err)
	}
	if got := e.srv.ClientState(ph.SessionID).Received; got[len(got)-1] != "Pause" {
		t.Errorf("phone received %v", got)
	}
	if _, ok := e.sink.Session(s.ID); !ok {
		t.Fatal("session removed during the grace")
	}
	e.srv.Update(ph.SessionID, func(c *fakejellyfin.Client) { c.Hidden = false })
	e.prov.Refresh()
	time.Sleep(150 * time.Millisecond)
	if rm := e.sink.Removes(); len(rm) != 0 {
		t.Fatalf("a blip removed the session: %v", rm)
	}
	q, _ := e.prov.Queue(ctx, s.ID)
	if q.Context == nil || q.Context.ID != pl || q.CurrentIndex == nil || *q.CurrentIndex != 0 {
		t.Errorf("context lost in the blip: %+v current %v", q.Context, q.CurrentIndex)
	}
	l := logs.String()
	if !strings.Contains(l, `msg="session missing from poll"`) || !strings.Contains(l, "client=Finamp") || !strings.Contains(l, `msg="session back in poll"`) ||
		strings.Count(l, `msg="session missing from poll"`) != 1 {
		t.Errorf("logs: %s", l)
	}

	// Missing for longer than the grace: removed once, context ended.
	e.srv.Update(ph.SessionID, func(c *fakejellyfin.Client) { c.Hidden = true })
	e.sink.WaitFor(t, wait, "removal", func(sk *providertest.Sink) bool { return len(sk.Removes()) > 0 })
	time.Sleep(200 * time.Millisecond)
	if rm := e.sink.Removes(); len(rm) != 1 || rm[0] != s.ID {
		t.Errorf("removes %v", rm)
	}
	if q, err := e.prov.Queue(ctx, s.ID); err == nil || q.Context != nil {
		t.Errorf("the context outlived the removal: %+v %v", q.Context, err)
	}
	l = logs.String()
	if !strings.Contains(l, `msg="session removed after missing from polls"`) || !strings.Contains(l, `reason="session gone"`) {
		t.Errorf("logs: %s", l)
	}
}

func TestCommandToMissingSessionGoneFromServer(t *testing.T) {
	e := graceEnv(t, time.Minute, nil)
	library(e.srv)
	ph := queuePhone(e.srv)
	e.login(t)
	e.run(t)
	s := e.sessionFor(t, "iphone-q", "Finamp", nil)
	e.srv.RemoveClient(ph.SessionID) // the server answers 404 now
	time.Sleep(150 * time.Millisecond)
	if _, ok := e.sink.Session(s.ID); !ok {
		t.Fatal("removed before the grace")
	}
	err := e.prov.Command(context.Background(), s.ID, model.Command{Name: model.CmdPause})
	if model.CodeOf(err) != model.ErrSessionUnavailable {
		t.Errorf("want session_unavailable, got %v", err)
	}
}

func TestMissingSessionRemovedAtOnceWithoutGrace(t *testing.T) {
	e := graceEnv(t, 0, nil)
	library(e.srv)
	ph := queuePhone(e.srv)
	e.login(t)
	e.run(t)
	s := e.sessionFor(t, "iphone-q", "Finamp", nil)
	e.srv.Update(ph.SessionID, func(c *fakejellyfin.Client) { c.Hidden = true })
	e.sink.WaitFor(t, wait, "removal", func(sk *providertest.Sink) bool { return len(sk.Removes()) == 1 && sk.Removes()[0] == s.ID })
}
