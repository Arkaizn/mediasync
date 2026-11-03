package fakejellyfin

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Item is a library item (track, album or playlist).
type Item struct {
	ID           string
	Name         string
	Type         string // "Audio", "MusicAlbum", "Playlist"
	Album        string
	AlbumID      string
	Artists      []string
	RunTimeTicks int64
	ImageTag     string
	// Children are the entries of an album or playlist, in order.
	Children []string
	// Lyrics, if non-nil, are served by /Audio/{id}/Lyrics.
	Lyrics []LyricLine
}

// LyricLine is one lyric line; StartTicks nil means unsynced.
type LyricLine struct {
	Text       string
	StartTicks *int64
}

// AddItem adds a library item.
func (s *Server) AddItem(it Item) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items == nil {
		s.items = map[string]*Item{}
	}
	cp := it
	s.items[it.ID] = &cp
	s.itemOrder = append(s.itemOrder, it.ID)
}

func playable(c *Client) []string {
	if c.PlayableTypes != nil {
		return c.PlayableTypes
	}
	if c.Controllable {
		return []string{"Audio"}
	}
	return []string{}
}

func (s *Server) queueJSON(c *Client) []map[string]any {
	out := []map[string]any{}
	if !s.StoreQueue {
		return out
	}
	for _, e := range c.Queue {
		out = append(out, map[string]any{"Id": e.ID, "PlaylistItemId": e.PlaylistItemID})
	}
	return out
}

func (s *Server) itemJSON(it *Item) map[string]any {
	m := map[string]any{
		"Id": it.ID, "Name": it.Name, "Type": it.Type, "Album": it.Album, "AlbumId": it.AlbumID,
		"Artists": it.Artists, "ImageTags": map[string]string{}, "MediaType": "Audio",
	}
	if it.RunTimeTicks > 0 {
		m["RunTimeTicks"] = it.RunTimeTicks
	}
	if it.ImageTag != "" {
		m["ImageTags"] = map[string]string{"Primary": it.ImageTag}
	}
	if it.Type == "Audio" {
		m["HasLyrics"] = it.Lyrics != nil
	}
	if it.Type == "Playlist" || it.Type == "MusicAlbum" {
		m["ChildCount"] = len(it.Children)
	}
	return m
}

func (s *Server) itemsHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []map[string]any{}
	switch {
	case q.Get("ids") != "":
		for _, id := range strings.Split(q.Get("ids"), ",") {
			if it := s.items[id]; it != nil {
				out = append(out, s.itemJSON(it))
			}
		}
	case q.Get("includeItemTypes") != "":
		types := strings.Split(q.Get("includeItemTypes"), ",")
		var names []*Item
		for _, id := range s.itemOrder {
			it := s.items[id]
			for _, t := range types {
				if strings.EqualFold(t, it.Type) {
					names = append(names, it)
				}
			}
		}
		sort.SliceStable(names, func(i, j int) bool { return names[i].Name < names[j].Name })
		for _, it := range names {
			out = append(out, s.itemJSON(it))
		}
	}
	writeJSON(w, map[string]any{"Items": out, "TotalRecordCount": len(out)})
}

func (s *Server) playlistItems(w http.ResponseWriter, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pl := s.items[id]
	if pl == nil || pl.Type != "Playlist" {
		http.NotFound(w, nil)
		return
	}
	out := []map[string]any{}
	for _, cid := range pl.Children {
		if it := s.items[cid]; it != nil {
			m := s.itemJSON(it)
			m["PlaylistItemId"] = cid
			out = append(out, m)
		}
	}
	writeJSON(w, map[string]any{"Items": out, "TotalRecordCount": len(out)})
}

func (s *Server) lyricsHandler(w http.ResponseWriter, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it := s.items[id]
	if it == nil || it.Lyrics == nil {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"title":"Not Found","status":404}`))
		return
	}
	lines := []map[string]any{}
	for _, l := range it.Lyrics {
		m := map[string]any{"Text": l.Text}
		if l.StartTicks != nil {
			m["Start"] = *l.StartTicks
			m["Cues"] = []any{}
		}
		lines = append(lines, m)
	}
	writeJSON(w, map[string]any{"Metadata": map[string]any{}, "Lyrics": lines})
}

// expand mimics the server: albums and playlists become their tracks;
// playlists are reordered by name (the real 12.2 server does not keep
// playlist order when expanding); unknown ids are dropped silently.
func (s *Server) expand(ids []string) []string {
	var out []string
	for _, id := range ids {
		it := s.items[id]
		if it == nil {
			continue
		}
		switch it.Type {
		case "Playlist":
			children := append([]string(nil), it.Children...)
			sort.SliceStable(children, func(i, j int) bool { return s.items[children[i]].Name < s.items[children[j]].Name })
			out = append(out, children...)
		case "MusicAlbum":
			out = append(out, it.Children...)
		default:
			out = append(out, id)
		}
	}
	return out
}

// play handles POST /Sessions/{id}/Playing (s.mu held).
func (s *Server) play(w http.ResponseWriter, r *http.Request, c *Client, ok bool) {
	q := r.URL.Query()
	pc := q.Get("playCommand")
	if pc != "PlayNow" && pc != "PlayNext" && pc != "PlayLast" && pc != "PlayShuffle" && pc != "PlayInstantMix" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if !ok {
		http.Error(w, "Error processing request.", http.StatusNotFound)
		return
	}
	var ids []string
	if v := q.Get("itemIds"); v != "" {
		ids = s.expand(strings.Split(v, ","))
	}
	start := 0
	if v := q.Get("startIndex"); v != "" {
		start, _ = strconv.Atoi(v)
	}
	// Like the real server: 204 even without a control connection.
	w.WriteHeader(http.StatusNoContent)
	if !c.Controllable {
		return
	}
	c.Received = append(c.Received, fmt.Sprintf("Play(%s:%s@%d)", pc, strings.Join(ids, ","), start))
	if !c.ApplyCommands || len(ids) == 0 {
		return
	}
	s.syncPos(c)
	switch pc {
	case "PlayNow":
		if start >= len(ids) {
			start = 0
		}
		s.setCurrent(c, ids[start])
		c.Queue = nil
		for _, id := range ids[start+1:] {
			c.Queue = append(c.Queue, QueueEntry{ID: id, PlaylistItemID: "queue"})
		}
	case "PlayNext":
		var add []QueueEntry
		for _, id := range ids {
			add = append(add, QueueEntry{ID: id, PlaylistItemID: "nextUp"})
		}
		c.Queue = append(add, c.Queue...)
	case "PlayLast":
		for _, id := range ids {
			c.Queue = append(c.Queue, QueueEntry{ID: id, PlaylistItemID: "queue"})
		}
	}
}

func (s *Server) setCurrent(c *Client, id string) {
	it := s.items[id]
	c.ItemID = id
	c.PositionTicks = 0
	c.Stopped = false
	if it != nil {
		c.Title, c.Album, c.AlbumID, c.Artists, c.RunTimeTicks = it.Name, it.Album, it.AlbumID, it.Artists, it.RunTimeTicks
	}
}
