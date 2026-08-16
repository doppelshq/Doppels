package telemetry_test

import (
	"testing"
	"time"

	"doppels.so/cli/internal/configstore"
	"doppels.so/cli/internal/telemetry"
)

func TestDecideEnvZeroWins(t *testing.T) {
	got := telemetry.Decide([]string{"DOPPELS_TELEMETRY=0"}, configstore.Telemetry{Enabled: true})
	if got.Enabled || got.Source != "env" {
		t.Fatalf("got %#v", got)
	}
}

func TestDecideProfileOptIn(t *testing.T) {
	got := telemetry.Decide(nil, configstore.Telemetry{Enabled: true})
	if !got.Enabled || got.Source != "profile" {
		t.Fatalf("got %#v", got)
	}
}

func TestDecideProfileOptOut(t *testing.T) {
	got := telemetry.Decide(nil, configstore.Telemetry{
		Enabled:    false,
		RejectedAt: time.Date(2026, 8, 16, 18, 0, 0, 0, time.UTC),
	})
	if got.Enabled || got.Source != "profile" {
		t.Fatalf("got %#v", got)
	}
}

func TestDecideDefaultOff(t *testing.T) {
	got := telemetry.Decide(nil, configstore.Telemetry{})
	if got.Enabled || got.Source != "default" {
		t.Fatalf("got %#v", got)
	}
}

func TestTrackNoopWhenDisabled(t *testing.T) {
	client := telemetry.NewClient(telemetry.Decision{Enabled: false})
	client.Track("cli.started", map[string]any{"version": "test"})
	if client.Enabled() {
		t.Fatal("expected disabled client")
	}
}
