package model

// CommandName identifies a playback command.
type CommandName string

const (
	CmdPlay      CommandName = "play"
	CmdPause     CommandName = "pause"
	CmdPlayPause CommandName = "play_pause"
	CmdStop      CommandName = "stop"
	CmdNext      CommandName = "next"
	CmdPrevious  CommandName = "previous"
	// CmdSeek seeks to an absolute position (PositionMS) or by a relative
	// offset (OffsetMS). Exactly one must be set.
	CmdSeek CommandName = "seek"
	// CmdSetVolume sets the volume to Volume (0.0–1.0).
	CmdSetVolume CommandName = "set_volume"
	// CmdQueueJump plays the queue item at Index (see sessions.queue).
	CmdQueueJump CommandName = "queue_jump"
	// CmdPlayItems plays tracks, albums or playlists (ItemIDs) now, next or last (Mode).
	CmdPlayItems CommandName = "play_items"
)

// Play modes for CmdPlayItems.
const (
	PlayModeNow  = "now"
	PlayModeNext = "next"
	PlayModeLast = "last"
)

// MaxPlayItems bounds the number of item ids in one play_items command.
const MaxPlayItems = 200

// Command is a playback command addressed to one session.
type Command struct {
	Name       CommandName `json:"command"`
	PositionMS *int64      `json:"position_ms,omitempty"`
	OffsetMS   *int64      `json:"offset_ms,omitempty"`
	Volume     *float64    `json:"volume,omitempty"`
	Index      *int        `json:"index,omitempty"`
	ItemIDs    []string    `json:"item_ids,omitempty"`
	Mode       string      `json:"mode,omitempty"`
}

// RequiredCapability returns the capability a command needs.
func (c Command) RequiredCapability() (Capability, bool) {
	switch c.Name {
	case CmdPlay:
		return CapPlay, true
	case CmdPause:
		return CapPause, true
	case CmdPlayPause:
		return CapPlayPause, true
	case CmdStop:
		return CapStop, true
	case CmdNext:
		return CapNext, true
	case CmdPrevious:
		return CapPrevious, true
	case CmdSeek:
		return CapSeek, true
	case CmdSetVolume:
		return CapVolume, true
	case CmdQueueJump:
		return CapQueueJump, true
	case CmdPlayItems:
		return CapPlayItems, true
	}
	return "", false
}

// Validate checks the command arguments.
func (c Command) Validate() error {
	if _, ok := c.RequiredCapability(); !ok {
		return Errorf(ErrInvalidArgument, "unknown command %q", c.Name)
	}
	switch c.Name {
	case CmdSeek:
		if (c.PositionMS == nil) == (c.OffsetMS == nil) {
			return Errorf(ErrInvalidArgument, "seek requires exactly one of position_ms or offset_ms")
		}
		if c.PositionMS != nil && *c.PositionMS < 0 {
			return Errorf(ErrInvalidArgument, "position_ms must be >= 0")
		}
	case CmdSetVolume:
		if c.Volume == nil {
			return Errorf(ErrInvalidArgument, "set_volume requires volume")
		}
		if *c.Volume < 0 || *c.Volume > 1.5 {
			return Errorf(ErrInvalidArgument, "volume must be between 0.0 and 1.5")
		}
	case CmdQueueJump:
		if c.Index == nil || *c.Index < 0 {
			return Errorf(ErrInvalidArgument, "queue_jump requires index >= 0")
		}
		if c.PositionMS != nil || c.OffsetMS != nil || c.Volume != nil || c.ItemIDs != nil || c.Mode != "" {
			return Errorf(ErrInvalidArgument, "queue_jump only takes index")
		}
		return nil
	case CmdPlayItems:
		if len(c.ItemIDs) == 0 || len(c.ItemIDs) > MaxPlayItems {
			return Errorf(ErrInvalidArgument, "play_items requires 1 to %d item_ids", MaxPlayItems)
		}
		for _, id := range c.ItemIDs {
			if !validItemID(id) {
				return Errorf(ErrInvalidArgument, "invalid item id %q", id)
			}
		}
		switch c.Mode {
		case "", PlayModeNow, PlayModeNext, PlayModeLast:
		default:
			return Errorf(ErrInvalidArgument, "mode must be now, next or last")
		}
		if c.PositionMS != nil || c.OffsetMS != nil || c.Volume != nil || c.Index != nil {
			return Errorf(ErrInvalidArgument, "play_items only takes item_ids and mode")
		}
		return nil
	default:
		if c.PositionMS != nil || c.OffsetMS != nil || c.Volume != nil {
			return Errorf(ErrInvalidArgument, "command %q takes no arguments", c.Name)
		}
	}
	if c.Index != nil || c.ItemIDs != nil || c.Mode != "" {
		return Errorf(ErrInvalidArgument, "command %q does not take index, item_ids or mode", c.Name)
	}
	return nil
}

// PlayMode returns the play_items mode with the default applied.
func (c Command) PlayMode() string {
	if c.Mode == "" {
		return PlayModeNow
	}
	return c.Mode
}

// validItemID accepts provider item ids: 1–64 characters of [A-Za-z0-9-_].
func validItemID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_'
		if !ok {
			return false
		}
	}
	return true
}

// Outcome describes how far a command's success was established.
type Outcome string

const (
	// OutcomeVerified means the expected state change was observed.
	OutcomeVerified Outcome = "verified"
	// OutcomeAcknowledged means the provider accepted the command, but the
	// expected effect was not observed within the verification window (or is
	// not observable, e.g. "next" on the last track of a queue).
	OutcomeAcknowledged Outcome = "acknowledged"
)

// CommandResult is returned for a successfully dispatched command.
type CommandResult struct {
	SessionID string      `json:"session_id"`
	Command   CommandName `json:"command"`
	Outcome   Outcome     `json:"outcome"`
	// Detail explains an acknowledged outcome.
	Detail string `json:"detail,omitempty"`
	// ViaSessionID is set when a command for a local session was carried
	// out through its linked remote session (queue_jump, play_items).
	ViaSessionID string   `json:"via_session_id,omitempty"`
	Session      *Session `json:"session,omitempty"`
}
