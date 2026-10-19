package model

import "time"

// ProviderState is the health state of a provider instance.
type ProviderState string

const (
	ProviderStarting        ProviderState = "starting"
	ProviderRunning         ProviderState = "running"
	ProviderDisabled        ProviderState = "disabled"
	ProviderUnauthenticated ProviderState = "unauthenticated"
	ProviderDegraded        ProviderState = "degraded"
	ProviderError           ProviderState = "error"
	ProviderStopped         ProviderState = "stopped"
)

// ProviderStatus reports the health of a provider instance.
type ProviderStatus struct {
	Instance string        `json:"instance"`
	Kind     string        `json:"kind"`
	Origin   Origin        `json:"origin"`
	State    ProviderState `json:"state"`
	// Message is a human readable explanation (never contains secrets).
	Message     string     `json:"message,omitempty"`
	LastError   *ErrorInfo `json:"last_error"`
	LastSuccess *time.Time `json:"last_success"`
	Sessions    int        `json:"sessions"`
	// Details holds provider-specific, non-secret diagnostic data.
	Details map[string]any `json:"details,omitempty"`
}
