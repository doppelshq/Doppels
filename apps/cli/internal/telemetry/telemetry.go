// Package telemetry decides whether anonymous product analytics may ship.
// The transport is intentionally a no-op until PostHog (or similar) is wired.
package telemetry

import (
	"strings"

	"doppels.so/cli/internal/configstore"
)

// Decision is the effective telemetry preference after env + profile.
type Decision struct {
	Enabled bool
	// Source is "env", "profile", or "default".
	Source string
}

// Decide returns whether events may be sent. DOPPELS_TELEMETRY=0|false|off
// always wins and disables. =1|true|on forces enable for the process.
// Otherwise the persisted profile opt-in applies (default off).
func Decide(environment []string, prefs configstore.Telemetry) Decision {
	switch strings.ToLower(strings.TrimSpace(envValue(environment, "DOPPELS_TELEMETRY"))) {
	case "0", "false", "off", "no":
		return Decision{Enabled: false, Source: "env"}
	case "1", "true", "on", "yes":
		return Decision{Enabled: true, Source: "env"}
	}
	if prefs.Enabled {
		return Decision{Enabled: true, Source: "profile"}
	}
	if !prefs.AcceptedAt.IsZero() || !prefs.RejectedAt.IsZero() {
		return Decision{Enabled: false, Source: "profile"}
	}
	return Decision{Enabled: false, Source: "default"}
}

// Client is a future event sink. Track is a no-op until a backend is configured.
type Client struct {
	enabled bool
}

func NewClient(decision Decision) *Client {
	return &Client{enabled: decision.Enabled}
}

func (c *Client) Enabled() bool {
	return c != nil && c.enabled
}

// Track records a named event. No network I/O yet.
func (c *Client) Track(_ string, _ map[string]any) {
	if !c.Enabled() {
		return
	}
	// PostHog (or similar) will land here.
}

func envValue(environment []string, wanted string) string {
	prefix := wanted + "="
	for _, entry := range environment {
		if strings.HasPrefix(entry, prefix) {
			return entry[len(prefix):]
		}
	}
	return ""
}
