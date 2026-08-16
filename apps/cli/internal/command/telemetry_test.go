package command

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"doppels.so/cli/internal/configstore"
)

func telemetryTestApp(root string) (*App, *bytes.Buffer, *bytes.Buffer) {
	app, stdout, stderr := testApp(root)
	configHome := filepath.Join(root, ".test-config")
	app.Environment = []string{"DOPPELS_CONFIG_HOME=" + configHome}
	return app, stdout, stderr
}

func TestTelemetryAcceptPersistsOptIn(t *testing.T) {
	root := t.TempDir()
	app, stdout, stderr := telemetryTestApp(root)

	if code := app.Run([]string{"telemetry", "accept"}); code != ExitSuccess {
		t.Fatalf("accept exit = %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Telemetry enabled") {
		t.Fatalf("accept stdout = %q", stdout.String())
	}

	store := configstore.New(filepath.Join(root, ".test-config"))
	profile, err := store.Profile()
	if err != nil {
		t.Fatal(err)
	}
	if !profile.Telemetry.Enabled {
		t.Fatalf("telemetry not enabled in profile: %#v", profile.Telemetry)
	}
	if profile.Telemetry.AcceptedAt.IsZero() {
		t.Fatal("acceptedAt not set")
	}
}

func TestTelemetryRejectAndStatusJSON(t *testing.T) {
	root := t.TempDir()
	app, stdout, stderr := telemetryTestApp(root)

	if code := app.Run([]string{"telemetry", "accept"}); code != ExitSuccess {
		t.Fatalf("accept exit = %d stderr=%s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()

	if code := app.Run([]string{"telemetry", "reject"}); code != ExitSuccess {
		t.Fatalf("reject exit = %d stderr=%s", code, stderr.String())
	}
	stdout.Reset()

	if code := app.Run([]string{"telemetry", "status", "--json"}); code != ExitSuccess {
		t.Fatalf("status exit = %d stderr=%s", code, stderr.String())
	}
	var payload struct {
		Kind    string `json:"kind"`
		Enabled bool   `json:"enabled"`
		Source  string `json:"source"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode status: %v\n%s", err, stdout.String())
	}
	if payload.Kind != "TelemetryStatus" || payload.Enabled || payload.Source != "profile" {
		t.Fatalf("status = %#v", payload)
	}
}

func TestTelemetryEnvZeroOverridesAccept(t *testing.T) {
	root := t.TempDir()
	app, stdout, stderr := telemetryTestApp(root)
	app.Environment = append(app.Environment, "DOPPELS_TELEMETRY=0")

	if code := app.Run([]string{"telemetry", "accept"}); code != ExitSuccess {
		t.Fatalf("accept exit = %d stderr=%s", code, stderr.String())
	}
	stdout.Reset()

	if code := app.Run([]string{"telemetry", "status", "--json"}); code != ExitSuccess {
		t.Fatalf("status exit = %d stderr=%s", code, stderr.String())
	}
	var payload struct {
		Enabled bool   `json:"enabled"`
		Source  string `json:"source"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Enabled || payload.Source != "env" {
		t.Fatalf("status with DOPPELS_TELEMETRY=0 = %#v", payload)
	}
}
