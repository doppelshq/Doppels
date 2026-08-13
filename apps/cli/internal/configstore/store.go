// Package configstore persists the CLI profile, selected control-plane context,
// and Space fulfillment bindings outside a Space working tree. Credentials are
// kept in a separate, owner-only file so ordinary context inspection never reads
// or prints them.
package configstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"time"
)

const version = 1

// Offline control-plane names (no cloud).
const (
	LocalOrganization = "local"
	LocalSpace        = "private"
)

var scopeName = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[-_][a-z0-9]+)*$`)

type Context struct {
	Organization string `json:"organization,omitempty"`
	Space        string `json:"space,omitempty"`
}

func (c Context) String() string {
	if c.Space == "" {
		return c.Organization
	}
	return c.Organization + "/" + c.Space
}

func (c Context) Valid() bool {
	return validScopeName(c.Organization) && (c.Space == "" || validScopeName(c.Space))
}

func (c Context) IsLocal() bool {
	return c.Organization == LocalOrganization
}

func LocalContext() Context {
	return Context{Organization: LocalOrganization, Space: LocalSpace}
}

func validScopeName(value string) bool { return len(value) <= 63 && scopeName.MatchString(value) }

// SpaceBinding records which absolute path fulfills a cloud or local Space.
type SpaceBinding struct {
	Organization string `json:"organization"`
	Space        string `json:"space"`
	Path         string `json:"path"`
}

type Profile struct {
	Server   string         `json:"server,omitempty"`
	Context  Context        `json:"context,omitempty"`
	LoginAt  time.Time      `json:"loginAt,omitempty"`
	Bindings []SpaceBinding `json:"bindings,omitempty"`
}

type credentials struct {
	Version int    `json:"version"`
	Server  string `json:"server"`
	Token   string `json:"token"`
}

type profileFile struct {
	Version int     `json:"version"`
	Profile Profile `json:"profile"`
}

type Session struct {
	Profile Profile
	Token   string
}

type Store struct{ dir string }

func DefaultDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "doppels")
	legacy := filepath.Join(base, "doppel")
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		if info, legacyErr := os.Stat(legacy); legacyErr == nil && info.IsDir() {
			if renameErr := os.Rename(legacy, dir); renameErr != nil {
				return legacy, nil
			}
		}
	}
	return dir, nil
}

func New(dir string) *Store { return &Store{dir: dir} }

func (s *Store) Login(server, token string, now time.Time) error {
	if server == "" || token == "" {
		return errors.New("server and token are required")
	}
	profile, err := s.Profile()
	if err != nil && !errors.Is(err, ErrNotConfigured) {
		return err
	}
	if profile.Server != "" && profile.Server != server {
		// A Context is scoped to one control plane. Carrying it to another
		// endpoint could apply to an unintended Organization/Space with the
		// same names.
		profile.Context = Context{}
		profile.Bindings = nil
	}
	profile.Server = server
	profile.LoginAt = now.UTC()
	if err := s.writeJSON("profile.json", profileFile{Version: version, Profile: profile}); err != nil {
		return err
	}
	if err := s.writeJSON("credentials.json", credentials{Version: version, Server: server, Token: token}); err != nil {
		return err
	}
	return nil
}

var (
	ErrNotLoggedIn   = errors.New("not logged in; run doppels login")
	ErrNotConfigured = errors.New("CLI profile not configured; run doppels init or doppels login")
)

func (s *Store) Session() (Session, error) {
	profile, err := s.Profile()
	if err != nil {
		return Session{}, err
	}
	if profile.Server == "" {
		return Session{}, ErrNotLoggedIn
	}
	var secret credentials
	if err := s.readJSON("credentials.json", &secret); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Session{}, ErrNotLoggedIn
		}
		return Session{}, err
	}
	if secret.Version != version || secret.Token == "" || secret.Server == "" {
		return Session{}, errors.New("invalid credentials file; run doppels login again")
	}
	if secret.Server != profile.Server {
		return Session{}, errors.New("profile and credentials belong to different servers; run doppels login again")
	}
	return Session{Profile: profile, Token: secret.Token}, nil
}

func (s *Store) Profile() (Profile, error) {
	var document profileFile
	if err := s.readJSON("profile.json", &document); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Profile{}, ErrNotConfigured
		}
		return Profile{}, err
	}
	if document.Version != version {
		return Profile{}, errors.New("invalid profile file; run doppels init or doppels login again")
	}
	return document.Profile, nil
}

func (s *Store) Context() (Context, error) {
	profile, err := s.Profile()
	if err != nil {
		return Context{}, err
	}
	if !profile.Context.Valid() {
		return Context{}, errors.New("no context selected; run doppels org use <organization> and doppels space use <space>")
	}
	return profile.Context, nil
}

func (s *Store) SetContext(context Context) error {
	if !context.Valid() {
		return errors.New("context names must be lowercase identifiers")
	}
	if !context.IsLocal() {
		if _, err := s.Session(); err != nil {
			return err
		}
	}
	profile, err := s.Profile()
	if err != nil {
		if !errors.Is(err, ErrNotConfigured) || !context.IsLocal() {
			return err
		}
		profile = Profile{}
	}
	profile.Context = context
	return s.writeJSON("profile.json", profileFile{Version: version, Profile: profile})
}

// EnsureLocalContext writes offline profile context local/private when missing.
func (s *Store) EnsureLocalContext() error {
	profile, err := s.Profile()
	if err != nil && !errors.Is(err, ErrNotConfigured) {
		return err
	}
	if errors.Is(err, ErrNotConfigured) {
		profile = Profile{Context: LocalContext()}
		return s.writeJSON("profile.json", profileFile{Version: version, Profile: profile})
	}
	if !profile.Context.Valid() {
		profile.Context = LocalContext()
		return s.writeJSON("profile.json", profileFile{Version: version, Profile: profile})
	}
	return nil
}

func (s *Store) SetBinding(organization, space, path string) error {
	if !validScopeName(organization) || !validScopeName(space) {
		return errors.New("binding names must be lowercase identifiers")
	}
	absolute, absErr := filepath.Abs(path)
	if absErr != nil {
		return absErr
	}
	profile, err := s.Profile()
	if err != nil {
		return err
	}
	updated := make([]SpaceBinding, 0, len(profile.Bindings)+1)
	for _, binding := range profile.Bindings {
		if binding.Organization == organization && binding.Space == space {
			continue
		}
		updated = append(updated, binding)
	}
	updated = append(updated, SpaceBinding{Organization: organization, Space: space, Path: absolute})
	profile.Bindings = updated
	return s.writeJSON("profile.json", profileFile{Version: version, Profile: profile})
}

func (s *Store) Binding(organization, space string) (SpaceBinding, bool, error) {
	profile, err := s.Profile()
	if err != nil {
		return SpaceBinding{}, false, err
	}
	for _, binding := range profile.Bindings {
		if binding.Organization == organization && binding.Space == space {
			return binding, true, nil
		}
	}
	return SpaceBinding{}, false, nil
}

func (s *Store) Logout() error {
	path := filepath.Join(s.dir, "credentials.json")
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (s *Store) readJSON(name string, output any) error {
	path := filepath.Join(s.dir, name)
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
		return fmt.Errorf("%s must be a regular owner-only file", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, output); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func (s *Store) writeJSON(name string, value any) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(s.dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(s.dir, ".doppels-config-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, filepath.Join(s.dir, name))
}
