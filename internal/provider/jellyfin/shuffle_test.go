package jellyfin

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/arkaizn/mediasync/internal/model"
	"github.com/arkaizn/mediasync/internal/testutil/fakejellyfin"
)

func lastPlayIDs(t *testing.T, e *env) []string {
	t.Helper()
	reqs := playRequests(e.srv)
	if len(reqs) == 0 {
		t.Fatal("no play command sent")
	}
	ids, _ := sentIDs(t, reqs[len(reqs)-1])
	return ids
}

func sortedCopy(ids []string) []string {
	out := slices.Clone(ids)
	slices.Sort(out)
	return out
}

func TestPlayItemsShuffleIsAPermutation(t *testing.T) {
	e := newEnv(t, nil)
	library(e.srv)
	plID, tracks := bigPlaylist(e.srv, 40)
	queuePhone(e.srv)
	e.login(t)
	e.run(t)
	s := e.sessionFor(t, "iphone-q", "Finamp", nil)
	if !s.Has(model.CapPlayItemsShuffle) {
		t.Fatalf("play_items_shuffle not advertised: %v", s.Capabilities)
	}
	ctx := context.Background()

	// A playlist and an album: with shuffle the album is expanded by
	// mediasync, so the whole list is shuffled together.
	want := append(slices.Clone(tracks), t1, t2, t3)
	differs := false
	for _, mode := range []string{"now", "next", "last", "now", "now"} {
		cmd := model.Command{Name: model.CmdPlayItems, ItemIDs: []string{plID, al}, Mode: mode, Shuffle: model.Ptr(true)}
		if err := e.prov.Command(ctx, s.ID, cmd); err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		got := lastPlayIDs(t, e)
		if !slices.Equal(sortedCopy(got), sortedCopy(want)) {
			t.Fatalf("%s: not a permutation of the input (%d ids, want %d)", mode, len(got), len(want))
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			differs = true
		}
		reqs := playRequests(e.srv)
		wantCmd := map[string]string{"now": "PlayNow", "next": "PlayNext", "last": "PlayLast"}[mode]
		if !strings.Contains(reqs[len(reqs)-1], "playCommand="+wantCmd) {
			t.Errorf("%s: sent %s", mode, reqs[len(reqs)-1])
		}
	}
	if !differs {
		t.Error("five shuffles of 43 tracks all kept the original order")
	}

	// Without shuffle nothing changes: playlist order, album passed through.
	if err := e.prov.Command(ctx, s.ID, model.Command{Name: model.CmdPlayItems, ItemIDs: []string{plID, al}}); err != nil {
		t.Fatal(err)
	}
	if got := lastPlayIDs(t, e); strings.Join(got, ",") != strings.Join(append(slices.Clone(tracks), al), ",") {
		t.Errorf("unshuffled order changed: %v", got)
	}
}

func TestPlayItemsShuffleRespectsURLBudget(t *testing.T) {
	e := newEnv(t, nil)
	library(e.srv)
	plID, tracks := bigPlaylist(e.srv, 249)
	queuePhone(e.srv)
	e.login(t)
	e.run(t)
	s := e.sessionFor(t, "iphone-q", "Finamp", nil)
	cmd := model.Command{Name: model.CmdPlayItems, ItemIDs: []string{plID}, Shuffle: model.Ptr(true)}
	if err := e.prov.Command(context.Background(), s.ID, cmd); err != nil {
		t.Fatal(err)
	}
	reqs := playRequests(e.srv)
	if len(reqs[len(reqs)-1]) > maxPlayRequestTarget {
		t.Errorf("request target %d bytes", len(reqs[len(reqs)-1]))
	}
	got := lastPlayIDs(t, e)
	seen := map[string]bool{}
	for _, id := range got {
		if seen[id] || !slices.Contains(tracks, id) {
			t.Fatalf("duplicate or foreign id %s", id)
		}
		seen[id] = true
	}
	// The cut is taken after shuffling: a random part, not the beginning.
	if len(got) >= len(tracks) || strings.Join(got, ",") == strings.Join(tracks[:len(got)], ",") {
		t.Errorf("sent %d ids; want a shuffled part of the playlist", len(got))
	}
}

func TestShuffledPlayItemsNeedsTheCapability(t *testing.T) {
	e := newEnv(t, nil)
	library(e.srv)
	e.srv.AddClient(&fakejellyfin.Client{ // a client without remote control
		UserID: e.srv.UserID("alice"), UserName: "alice", Client: "Jellyfin Web", DeviceName: "Firefox", DeviceID: "web-view",
		ItemID: t1, Title: "Track 1", RunTimeTicks: 1_800_000_000,
	})
	e.login(t)
	e.run(t)
	s := e.sessionFor(t, "web-view", "Jellyfin Web", nil)
	if s.Has(model.CapPlayItems) || s.Has(model.CapPlayItemsShuffle) {
		t.Fatalf("view-only client: %v", s.Capabilities)
	}
	err := e.prov.Command(context.Background(), s.ID, model.Command{Name: model.CmdPlayItems, ItemIDs: []string{t1}, Shuffle: model.Ptr(true)})
	if err == nil || len(playRequests(e.srv)) != 0 {
		t.Errorf("shuffled play_items reached a view-only client: %v", err)
	}
}

func TestShuffledPlayNowDoesNotStartWithTheCurrentTrack(t *testing.T) {
	e := newEnv(t, nil)
	library(e.srv)
	queuePhone(e.srv) // plays t1, which is on Album X with t2 and t3
	e.login(t)
	e.run(t)
	s := e.sessionFor(t, "iphone-q", "Finamp", nil)
	ctx := context.Background()
	for range 40 {
		cmd := model.Command{Name: model.CmdPlayItems, ItemIDs: []string{al}, Shuffle: model.Ptr(true)}
		if err := e.prov.Command(ctx, s.ID, cmd); err != nil {
			t.Fatal(err)
		}
		got := lastPlayIDs(t, e)
		if got[0] == t1 {
			t.Fatalf("shuffled play now starts with the playing track: %v", got)
		}
		if !slices.Equal(sortedCopy(got), sortedCopy([]string{t1, t2, t3})) {
			t.Fatalf("not a permutation: %v", got)
		}
		// The fake applies PlayNow; put t1 back as the current track.
		e.srv.Update(s.ProviderSessionID, func(c *fakejellyfin.Client) { c.ItemID = t1 })
		e.prov.Refresh()
		s = e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool { return *x.Track.TrackID == t1 })
	}
	// "next"/"last" keep any order: the current track keeps playing anyway.
}
