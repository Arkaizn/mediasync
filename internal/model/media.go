package model

// QueueItem is one entry of a session's play queue.
type QueueItem struct {
	Index      int      `json:"index"`
	ItemID     string   `json:"item_id"`
	Title      *string  `json:"title"`
	Artists    []string `json:"artists"`
	Album      *string  `json:"album"`
	DurationMS *int64   `json:"duration_ms"`
	ArtworkURL *string  `json:"artwork_url"`
}

// Queue is a session's play queue as returned by sessions.queue.
type Queue struct {
	SessionID string `json:"session_id"`
	// Available is false when no queue data exists for the session.
	Available    bool        `json:"available"`
	CurrentIndex *int        `json:"current_index"`
	Items        []QueueItem `json:"items"`
	Source       *string     `json:"source"`
}

// UnavailableQueue returns the queue result for sessions without queue data.
func UnavailableQueue(sessionID string) Queue {
	return Queue{SessionID: sessionID, Items: []QueueItem{}}
}

// Lyrics statuses.
const (
	LyricsOK          = "ok"
	LyricsNone        = "none"
	LyricsUnavailable = "unavailable"
)

// Lyrics sources.
const (
	LyricsSourceJellyfin = "jellyfin"
	LyricsSourceLRCLIB   = "lrclib"
	LyricsSourceMPRIS    = "mpris"
)

// LyricLine is one line of lyrics. StartMS is nil for unsynced lyrics.
type LyricLine struct {
	StartMS *int64 `json:"start_ms"`
	Text    string `json:"text"`
}

// Lyrics is the result of lyrics.get.
type Lyrics struct {
	SessionID string  `json:"session_id"`
	TrackID   *string `json:"track_id"`
	// Status is "ok", "none" (the source has no lyrics for this track) or
	// "unavailable" (cannot be determined).
	Status string      `json:"status"`
	Synced bool        `json:"synced"`
	Lines  []LyricLine `json:"lines"`
	Source *string     `json:"source"`
}

// Playlist is a server-side playlist.
type Playlist struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	ItemCount  *int    `json:"item_count"`
	ArtworkURL *string `json:"artwork_url"`
	Server     string  `json:"server"`
	MediaType  *string `json:"media_type"`
}
