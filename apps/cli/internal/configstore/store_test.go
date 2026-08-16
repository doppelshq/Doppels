package configstore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoginContextAndLogout(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "config"))
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	if err := store.Login("https://doppels.so", "secret-token", now); err != nil {
		t.Fatal(err)
	}
	if err := store.SetContext(Context{Organization: "acme", Space: "platform"}); err != nil {
		t.Fatal(err)
	}
	session, err := store.Session()
	if err != nil {
		t.Fatal(err)
	}
	if session.Token != "secret-token" || session.Profile.Context.String() != "acme/platform" {
		t.Fatalf("session = %#v", session)
	}
	for _, name := range []string{"profile.json", "credentials.json"} {
		info, err := os.Stat(filepath.Join(store.dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o", name, info.Mode().Perm())
		}
	}
	if err := store.Logout(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Session(); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("Session error = %v", err)
	}
	profile, err := store.Profile()
	if err != nil || profile.Context.String() != "acme/platform" {
		t.Fatalf("profile/context not preserved: %#v / %v", profile, err)
	}
}

func TestContextValidation(t *testing.T) {
	for _, context := range []Context{
		{}, {Organization: "Acme"}, {Organization: "acme", Space: "Platform"},
	} {
		if context.Valid() {
			t.Fatalf("context %#v should be invalid", context)
		}
	}
	if !(Context{Organization: "acme"}).Valid() || !(Context{Organization: "acme", Space: "platform-prod"}).Valid() {
		t.Fatal("valid contexts rejected")
	}
}

func TestLoginToAnotherServerClearsContext(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "config"))
	if err := store.Login("https://first.doppels.so", "first", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.SetContext(Context{Organization: "acme", Space: "platform"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Login("https://second.doppels.so", "second", time.Now()); err != nil {
		t.Fatal(err)
	}
	profile, err := store.Profile()
	if err != nil {
		t.Fatal(err)
	}
	if profile.Context != (Context{}) {
		t.Fatalf("Context leaked across servers: %#v", profile.Context)
	}
}

func TestEnsureLocalContextAndBinding(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "config"))
	if err := store.SetContext(LocalContext()); err != nil {
		t.Fatal(err)
	}
	current, err := store.Context()
	if err != nil || current.String() != "local/private" {
		t.Fatalf("context = %#v, %v", current, err)
	}
	if _, err := store.Session(); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("Session = %v", err)
	}
	root := t.TempDir()
	if err := store.SetBinding(LocalOrganization, LocalSpace, root); err != nil {
		t.Fatal(err)
	}
	binding, ok, err := store.Binding(LocalOrganization, LocalSpace)
	if err != nil || !ok || binding.Path == "" {
		t.Fatalf("binding = %#v, %t, %v", binding, ok, err)
	}
}

func TestSetTelemetryOptInWithoutLogin(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "config"))
	now := time.Date(2026, 8, 16, 18, 0, 0, 0, time.UTC)
	if err := store.SetTelemetry(true, now); err != nil {
		t.Fatal(err)
	}
	profile, err := store.Profile()
	if err != nil {
		t.Fatal(err)
	}
	if !profile.Telemetry.Enabled || !profile.Telemetry.AcceptedAt.Equal(now) {
		t.Fatalf("telemetry = %#v", profile.Telemetry)
	}
	if err := store.SetTelemetry(false, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	profile, err = store.Profile()
	if err != nil || profile.Telemetry.Enabled || profile.Telemetry.RejectedAt.IsZero() {
		t.Fatalf("reject = %#v / %v", profile.Telemetry, err)
	}
}

func TestSessionRejectsPartiallyUpdatedServerPair(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "config"))
	if err := store.Login("https://first.doppels.so", "first", time.Now()); err != nil {
		t.Fatal(err)
	}
	// Model a crash after profile.json was committed but before the matching
	// credentials.json rename. The old token must never travel to the new host.
	if err := store.writeJSON("profile.json", profileFile{Version: version, Profile: Profile{Server: "https://second.doppels.so"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Session(); err == nil || !strings.Contains(err.Error(), "different servers") {
		t.Fatalf("Session error = %v", err)
	}
}
