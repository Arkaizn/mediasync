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
)

// Command is a playback command addressed to one session.
type Command struct {
	Name       CommandName `json:"command"`
	PositionMS *int64      `json:"position_ms,omitempty"`
	OffsetMS   *int64      `json:"offset_ms,omitempty"`
	Volume     *float64    `json:"volume,omitempty"`
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
	default:
		if c.PositionMS != nil || c.OffsetMS != nil || c.Volume != nil {
			return Errorf(ErrInvalidArgument, "command %q takes no arguments", c.Name)
		}
	}
	return nil
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
	Detail  string   `json:"detail,omitempty"`
	Session *Session `json:"session,omitempty"`
}
