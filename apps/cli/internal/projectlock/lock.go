// Package projectlock manages the deterministic doppels.lock file that pins
// the exact immutable definitions last applied from a Project.
package projectlock

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"doppels.so/cli/internal/execution"
)

const Filename = "doppels.lock"

var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type Entry struct {
	Kind            string                        `json:"kind"`
	SourceAuthority string                        `json:"sourceAuthority"`
	Revision        execution.DefinitionReference `json:"revision"`
}

type File struct {
	APIVersion string  `json:"apiVersion"`
	Kind       string  `json:"kind"`
	Resources  []Entry `json:"resources"`
}

// Pending is a fully written lock file awaiting its final atomic rename. It
// lets callers prove the Project directory is writable before committing
// remote state.
type Pending struct {
	temporaryPath string
	targetPath    string
	committed     bool
}

func New(entries []Entry) File {
	copy := append([]Entry(nil), entries...)
	sort.Slice(copy, func(i, j int) bool { return key(copy[i]) < key(copy[j]) })
	return File{APIVersion: execution.APIVersion, Kind: "Lock", Resources: copy}
}

func Load(root string) (*File, error) {
	path := filepath.Join(root, Filename)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var lock File
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&lock); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode %s: trailing JSON", path)
	}
	if lock.APIVersion != execution.APIVersion || lock.Kind != "Lock" {
		return nil, fmt.Errorf("%s has an unsupported lock format", path)
	}
	seen := map[string]struct{}{}
	for _, entry := range lock.Resources {
		identifier := key(entry)
		if (entry.Kind != "Capability" && entry.Kind != "Recipe") ||
			(entry.SourceAuthority != "manifest" && entry.SourceAuthority != "cloud") ||
			entry.Revision.Name == "" || entry.Revision.Version == "" ||
			!digestPattern.MatchString(entry.Revision.ManifestSHA256) || entry.Revision.Schema.ID == "" ||
			!digestPattern.MatchString(entry.Revision.Schema.SHA256) {
			return nil, fmt.Errorf("%s contains an incomplete resource", path)
		}
		if _, exists := seen[identifier]; exists {
			return nil, fmt.Errorf("%s contains duplicate resource %s", path, identifier)
		}
		seen[identifier] = struct{}{}
	}
	return &lock, nil
}

// Verify rejects changing bytes, schema or authority under an already pinned
// kind/name@version. New revisions and resources are valid additions.
func Verify(existing *File, desired File) error {
	if existing == nil {
		return nil
	}
	pinned := make(map[string]Entry, len(existing.Resources))
	for _, entry := range existing.Resources {
		pinned[key(entry)] = entry
	}
	for _, entry := range desired.Resources {
		previous, exists := pinned[key(entry)]
		if !exists {
			continue
		}
		if previous.SourceAuthority != entry.SourceAuthority || previous.Revision.ManifestSHA256 != entry.Revision.ManifestSHA256 || previous.Revision.Schema != entry.Revision.Schema {
			return fmt.Errorf("%s changed without a version bump; update metadata.version", key(entry))
		}
	}
	return nil
}

func Write(root string, lock File) error {
	pending, err := Prepare(root, lock)
	if err != nil {
		return err
	}
	defer pending.Abort()
	return pending.Commit()
}

func Prepare(root string, lock File) (*Pending, error) {
	lock = New(lock.Resources)
	data, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(root, ".doppels-lock-*")
	if err != nil {
		return nil, err
	}
	temporaryPath := temporary.Name()
	preserve := false
	defer func() {
		if !preserve {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o644); err != nil {
		temporary.Close()
		return nil, err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return nil, err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return nil, err
	}
	if err := temporary.Close(); err != nil {
		return nil, err
	}
	preserve = true
	return &Pending{temporaryPath: temporaryPath, targetPath: filepath.Join(root, Filename)}, nil
}

func (pending *Pending) Commit() error {
	if pending == nil || pending.temporaryPath == "" || pending.committed {
		return errors.New("lock write is not pending")
	}
	if err := os.Rename(pending.temporaryPath, pending.targetPath); err != nil {
		return err
	}
	pending.committed = true
	return nil
}

func (pending *Pending) Abort() error {
	if pending == nil || pending.temporaryPath == "" || pending.committed {
		return nil
	}
	err := os.Remove(pending.temporaryPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func key(entry Entry) string {
	return entry.Kind + "/" + entry.Revision.Name + "@" + entry.Revision.Version
}
