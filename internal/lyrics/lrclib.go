package lyrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/arkaizn/mediasync/internal/model"
)

// LRCLIB fetches lyrics from an LRCLIB server (https://lrclib.net). It sends
// the track's artist, title, album and duration to that server, so it is
// only used when explicitly enabled.
type LRCLIB struct {
	BaseURL   string
	UserAgent string
	HTTP      *http.Client

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	lines  []model.LyricLine
	synced bool
	found  bool
	at     time.Time
}

const (
	lrclibCacheSize = 512
	lrclibCacheTTL  = 6 * time.Hour
)

// NewLRCLIB returns a client. baseURL defaults to https://lrclib.net.
func NewLRCLIB(baseURL, version string) *LRCLIB {
	if baseURL == "" {
		baseURL = "https://lrclib.net"
	}
	return &LRCLIB{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		UserAgent: "mediasync/" + version + " (https://github.com/arkaizn/mediasync)",
		HTTP:      &http.Client{Timeout: 10 * time.Second},
		cache:     map[string]cached{},
	}
}

// TrackInfo identifies a track for LRCLIB.
type TrackInfo struct {
	Artist     string
	Title      string
	Album      string
	DurationMS int64
}

// Get returns lyrics for the track. found is false when LRCLIB has no
// lyrics (or the track is instrumental). Errors mean the lookup failed.
func (c *LRCLIB) Get(ctx context.Context, t TrackInfo) (lines []model.LyricLine, synced, found bool, err error) {
	if t.Artist == "" || t.Title == "" {
		return nil, false, false, errors.New("lrclib needs artist and title")
	}
	key := strings.ToLower(t.Artist + "\x00" + t.Title + "\x00" + t.Album + "\x00" + fmt.Sprint(t.DurationMS/1000))
	c.mu.Lock()
	if e, ok := c.cache[key]; ok && time.Since(e.at) < lrclibCacheTTL {
		c.mu.Unlock()
		return e.lines, e.synced, e.found, nil
	}
	c.mu.Unlock()

	q := url.Values{"artist_name": {t.Artist}, "track_name": {t.Title}}
	if t.Album != "" {
		q.Set("album_name", t.Album)
	}
	if t.DurationMS > 0 {
		q.Set("duration", fmt.Sprint(t.DurationMS/1000))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/get?"+q.Encode(), nil)
	if err != nil {
		return nil, false, false, err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, false, false, err
	}
	defer resp.Body.Close()
	var e cached
	switch resp.StatusCode {
	case http.StatusOK:
		var body struct {
			Instrumental bool    `json:"instrumental"`
			PlainLyrics  *string `json:"plainLyrics"`
			SyncedLyrics *string `json:"syncedLyrics"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
			return nil, false, false, fmt.Errorf("lrclib: invalid response: %w", err)
		}
		switch {
		case body.Instrumental:
		case body.SyncedLyrics != nil && strings.TrimSpace(*body.SyncedLyrics) != "":
			e.lines, e.synced = ParseLRC(*body.SyncedLyrics)
			e.found = len(e.lines) > 0
		case body.PlainLyrics != nil && strings.TrimSpace(*body.PlainLyrics) != "":
			e.lines, _ = ParseLRC(*body.PlainLyrics)
			e.found = len(e.lines) > 0
		}
	case http.StatusNotFound:
	default:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, false, false, fmt.Errorf("lrclib: HTTP %d", resp.StatusCode)
	}
	e.at = time.Now()
	c.mu.Lock()
	if len(c.cache) >= lrclibCacheSize {
		c.cache = map[string]cached{}
	}
	c.cache[key] = e
	c.mu.Unlock()
	return e.lines, e.synced, e.found, nil
}
