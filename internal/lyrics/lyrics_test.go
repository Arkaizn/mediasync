package lyrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/arkaizn/mediasync/internal/model"
)

func texts(lines []model.LyricLine) []string {
	var out []string
	for _, l := range lines {
		out = append(out, l.Text)
	}
	return out
}

func TestParseLRCSynced(t *testing.T) {
	// The format Feishin publishes over MPRIS (xesam:asText).
	in := "[ar:Artist A]\n[ti:Track 1]\n[offset:0]\n[00:01.000]First line\n[00:05.500]Second\n[00:10.00]Third\n"
	lines, synced := ParseLRC(in)
	if !synced || len(lines) != 3 {
		t.Fatalf("synced=%v lines=%+v", synced, lines)
	}
	want := []int64{1000, 5500, 10000}
	for i, l := range lines {
		if l.StartMS == nil || *l.StartMS != want[i] {
			t.Errorf("line %d start %v", i, l.StartMS)
		}
	}
	if strings.Join(texts(lines), "|") != "First line|Second|Third" {
		t.Errorf("texts %v", texts(lines))
	}
}

func TestParseLRCRepeatedTagsOffsetAndWordTimes(t *testing.T) {
	in := "[offset:+500]\r\n[00:20.00][00:02.00]Chorus\r\n[00:10.5]<00:10.50>Word <00:11.00>timed\r\n[01:00.00]\r\n"
	lines, synced := ParseLRC(in)
	if !synced {
		t.Fatal("not synced")
	}
	// Sorted by time; offset +500 shows lines 0.5 s earlier; the empty
	// timed line is kept (an intentional pause).
	got := []string{}
	for _, l := range lines {
		got = append(got, l.Text+"@"+strconv.FormatInt(*l.StartMS, 10))
	}
	if strings.Join(got, ",") != "Chorus@1500,Word timed@10000,Chorus@19500,@59500" {
		t.Errorf("got %v", got)
	}
}

func TestParseLRCPlain(t *testing.T) {
	lines, synced := ParseLRC("\n\nPlain one\n\nPlain two\n\n")
	if synced {
		t.Fatal("plain text reported as synced")
	}
	if strings.Join(texts(lines), "|") != "Plain one||Plain two" {
		t.Errorf("got %q", texts(lines))
	}
	for _, l := range lines {
		if l.StartMS != nil {
			t.Error("unsynced line has a start")
		}
	}
	if lines, _ := ParseLRC("   "); len(lines) != 0 || lines == nil {
		t.Errorf("empty: %#v", lines)
	}
}

func TestLRCLIB(t *testing.T) {
	var hits atomic.Int32
	var lastUA, lastQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		lastUA, lastQuery = r.Header.Get("User-Agent"), r.URL.RawQuery
		if r.URL.Path != "/api/get" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		switch q.Get("track_name") {
		case "Synced":
			_, _ = w.Write([]byte(`{"id":1,"trackName":"Synced","instrumental":false,"plainLyrics":"a\nb","syncedLyrics":"[00:01.00]a\n[00:02.00]b"}`))
		case "Plain":
			_, _ = w.Write([]byte(`{"id":2,"instrumental":false,"plainLyrics":"only plain","syncedLyrics":null}`))
		case "Instrumental":
			_, _ = w.Write([]byte(`{"id":3,"instrumental":true,"plainLyrics":null,"syncedLyrics":null}`))
		case "Broken":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":404,"name":"TrackNotFound","message":"Failed to find specified track"}`))
		}
	}))
	defer srv.Close()
	c := NewLRCLIB(srv.URL, "1.2.3")
	ctx := context.Background()

	lines, synced, found, err := c.Get(ctx, TrackInfo{Artist: "A", Title: "Synced", Album: "X", DurationMS: 233_900})
	if err != nil || !found || !synced || len(lines) != 2 || *lines[1].StartMS != 2000 {
		t.Fatalf("synced: %+v %v %v %v", lines, synced, found, err)
	}
	if !strings.Contains(lastQuery, "duration=233") || !strings.Contains(lastQuery, "artist_name=A") || !strings.Contains(lastQuery, "album_name=X") {
		t.Errorf("query %q", lastQuery)
	}
	if !strings.HasPrefix(lastUA, "mediasync/1.2.3 ") {
		t.Errorf("user agent %q", lastUA)
	}
	// Cached: no second request.
	before := hits.Load()
	if _, _, found, _ := c.Get(ctx, TrackInfo{Artist: "a", Title: "synced", Album: "x", DurationMS: 233_100}); !found || hits.Load() != before {
		t.Error("cache miss")
	}
	if lines, synced, found, err := c.Get(ctx, TrackInfo{Artist: "A", Title: "Plain"}); err != nil || !found || synced || lines[0].Text != "only plain" {
		t.Errorf("plain: %+v %v %v %v", lines, synced, found, err)
	}
	if _, _, found, err := c.Get(ctx, TrackInfo{Artist: "A", Title: "Instrumental"}); err != nil || found {
		t.Errorf("instrumental: %v %v", found, err)
	}
	if _, _, found, err := c.Get(ctx, TrackInfo{Artist: "A", Title: "Missing"}); err != nil || found {
		t.Errorf("404: %v %v", found, err)
	}
	if _, _, _, err := c.Get(ctx, TrackInfo{Artist: "A", Title: "Broken"}); err == nil {
		t.Error("500 should be an error")
	}
	if _, _, _, err := c.Get(ctx, TrackInfo{Title: "No artist"}); err == nil {
		t.Error("missing artist should be an error")
	}
}
