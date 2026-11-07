package jellyfin

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/arkaizn/mediasync/internal/model"
	"github.com/arkaizn/mediasync/internal/testutil/fakejellyfin"
)

// bigPlaylist adds a playlist of n tracks (32-hex ids like real Jellyfin
// ids) and returns its id and the track ids in playlist order.
func bigPlaylist(srv *fakejellyfin.Server, n int) (string, []string) {
	var tracks []string
	for i := range n {
		id := fmt.Sprintf("%032x", 0xb000+i)
		srv.AddItem(fakejellyfin.Item{ID: id, Name: fmt.Sprintf("Song %03d", i), Type: "Audio", RunTimeTicks: 1_800_000_000})
		tracks = append(tracks, id)
	}
	plID := "b1g0000000000000000000000000p1a7"
	srv.AddItem(fakejellyfin.Item{ID: plID, Name: "Everything", Type: "Playlist", Children: tracks})
	return plID, tracks
}

// playRequests returns the request targets of the play commands sent so far.
func playRequests(srv *fakejellyfin.Server) []string {
	var out []string
	for _, r := range srv.Requests() {
		if strings.HasPrefix(r, "POST /Sessions/") && strings.Contains(r, "/Playing?") {
			out = append(out, strings.TrimPrefix(r, "POST "))
		}
	}
	return out
}

func sentIDs(t *testing.T, target string) ([]string, string) {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(u.Query().Get("itemIds"), ","), u.Query().Get("startIndex")
}

func TestPlayItemsLargePlaylistStaysWithinURLBudget(t *testing.T) {
	e := newEnv(t, nil)
	library(e.srv)
	plID, tracks := bigPlaylist(e.srv, 249) // the playlist from the bug report (≈8.3 KB of ids)
	queuePhone(e.srv)
	e.login(t)
	e.run(t)
	s := e.sessionFor(t, "iphone-q", "Finamp", nil)

	// A small playlist goes out complete.
	if err := e.prov.Command(context.Background(), s.ID, model.Command{Name: model.CmdPlayItems, ItemIDs: []string{pl}}); err != nil {
		t.Fatalf("small playlist: %v", err)
	}
	if ids, _ := sentIDs(t, playRequests(e.srv)[0]); strings.Join(ids, ",") != t3+","+t1 {
		t.Errorf("small playlist sent %v", ids)
	}

	// Without a budget the request line would be > 8 KiB and the server
	// (like Kestrel) would answer 414. With it, the command succeeds.
	if err := e.prov.Command(context.Background(), s.ID, model.Command{Name: model.CmdPlayItems, ItemIDs: []string{plID}}); err != nil {
		t.Fatalf("large playlist: %v", err)
	}
	reqs := playRequests(e.srv)
	last := reqs[len(reqs)-1]
	if len(reqs) != 2 {
		t.Fatalf("want one request per command, got %v", len(reqs))
	}
	if len(last) > maxPlayRequestTarget {
		t.Errorf("request target is %d bytes, budget %d", len(last), maxPlayRequestTarget)
	}
	ids, start := sentIDs(t, last)
	if start != "" {
		t.Errorf("play_items must not send a start index: %q", start)
	}
	// The beginning of the playlist, in playlist order, as much as fits.
	if len(ids) < 150 || len(ids) >= len(tracks) || strings.Join(ids, ",") != strings.Join(tracks[:len(ids)], ",") {
		t.Errorf("sent %d ids, want a playlist-order prefix of 150..248", len(ids))
	}
}

func TestQueueJumpOnLongQueueStaysWithinURLBudget(t *testing.T) {
	e := newEnv(t, nil)
	e.srv.SetStoreQueue(true)
	library(e.srv)
	_, tracks := bigPlaylist(e.srv, 400)
	ph := queuePhone(e.srv)
	e.srv.Update(ph.SessionID, func(c *fakejellyfin.Client) {
		c.ItemID = tracks[0]
		c.Queue = nil
		for _, id := range tracks[1:] {
			c.Queue = append(c.Queue, fakejellyfin.QueueEntry{ID: id, PlaylistItemID: "queue"})
		}
	})
	e.login(t)
	e.run(t)
	s := e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool { return x.Has(model.CapQueueJump) })

	for _, idx := range []int{0, 300, 399} {
		if err := e.prov.Command(context.Background(), s.ID, model.Command{Name: model.CmdQueueJump, Index: model.Ptr(idx)}); err != nil {
			t.Fatalf("jump %d: %v", idx, err)
		}
		reqs := playRequests(e.srv)
		last := reqs[len(reqs)-1]
		if len(last) > maxPlayRequestTarget {
			t.Errorf("jump %d: request target %d bytes", idx, len(last))
		}
		ids, start := sentIDs(t, last)
		var si int
		fmt.Sscan(start, &si)
		if si < 0 || si >= len(ids) || ids[si] != tracks[idx] {
			t.Errorf("jump %d: start index %s does not point at the target", idx, start)
		}
		// The window keeps queue order and is contiguous.
		first := strings.Index(strings.Join(tracks, ","), ids[0])
		if strings.Join(tracks, ",")[first:first+len(strings.Join(ids, ","))] != strings.Join(ids, ",") {
			t.Errorf("jump %d: window is not a contiguous part of the queue", idx)
		}
		// Re-read the session: the fake applied the jump, rebuild the queue.
		e.srv.Update(ph.SessionID, func(c *fakejellyfin.Client) {
			c.ItemID = tracks[0]
			c.Queue = nil
			for _, id := range tracks[1:] {
				c.Queue = append(c.Queue, fakejellyfin.QueueEntry{ID: id, PlaylistItemID: "queue"})
			}
		})
		e.prov.Refresh()
		s = e.sessionFor(t, "iphone-q", "Finamp", func(x model.Session) bool {
			return x.Has(model.CapQueueJump) && *x.Track.TrackID == tracks[0]
		})
	}
}

func TestPlayCommandRetriesForStricterProxyAndMapsTooLong(t *testing.T) {
	for _, status := range []int{http.StatusRequestURITooLong, http.StatusBadRequest} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			e := newEnv(t, nil)
			library(e.srv)
			plID, tracks := bigPlaylist(e.srv, 249)
			queuePhone(e.srv)
			e.login(t)
			e.run(t)
			s := e.sessionFor(t, "iphone-q", "Finamp", nil)

			// A proxy that only accepts 3000-byte request lines.
			e.srv.SetMaxRequestLine(3000, status)
			if err := e.prov.Command(context.Background(), s.ID, model.Command{Name: model.CmdPlayItems, ItemIDs: []string{plID}}); err != nil {
				t.Fatalf("want success after retrying with fewer items: %v", err)
			}
			reqs := playRequests(e.srv)
			ids, _ := sentIDs(t, reqs[len(reqs)-1])
			if len(reqs) < 2 || strings.Join(ids, ",") != strings.Join(tracks[:len(ids)], ",") {
				t.Errorf("retries %d, last sent %d ids", len(reqs), len(ids))
			}

			// Nothing fits (id lookups still pass): a clear error with the
			// HTTP status in its data.
			e.srv.SetMaxRequestLine(200, status)
			err := e.prov.Command(context.Background(), s.ID, model.Command{Name: model.CmdPlayItems, ItemIDs: []string{plID}})
			me := model.AsError(err)
			if err == nil || me.Code != model.ErrProviderError || !strings.Contains(me.Message, "too large for one command") || me.Data["http_status"] != status {
				t.Errorf("want mapped too-long error, got %v %v", err, me.Data)
			}
		})
	}
}

func TestPlayWindow(t *testing.T) {
	ids := []string{"aa", "bb", "cc", "dd", "ee"}
	// Each id costs 2, each separator 3.
	cases := []struct {
		index, budget, lo, hi int
	}{
		{0, 100, 0, 5},
		{0, 7, 0, 2},  // aa,bb
		{3, 7, 3, 5},  // dd,ee: following items first
		{4, 12, 2, 5}, // ee, then cc,dd before it
		{2, 1, 2, 3},  // the target is always sent
	}
	for _, c := range cases {
		if lo, hi := playWindow(ids, c.index, c.budget, 3); lo != c.lo || hi != c.hi {
			t.Errorf("playWindow(index %d, budget %d) = %d,%d want %d,%d", c.index, c.budget, lo, hi, c.lo, c.hi)
		}
	}
}
