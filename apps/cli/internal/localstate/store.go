// Package localstate persists the immutable local record of a Doppels Run.
package localstate

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

var safeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,199}$`)

// Store owns one directory under <project>/.doppels/runs/<run-id>.
// JSON documents and logs are committed with rename(2); events are appended
// with O_APPEND and fsynced before AppendEvent returns.
type Store struct {
	dir string
	mu  sync.Mutex
}

func Open(projectRoot, runID string) (*Store, error) {
	if !safeID.MatchString(runID) {
		return nil, fmt.Errorf("invalid run id %q", runID)
	}
	root, err := filepath.Abs(projectRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve project root: %w", err)
	}
	dir := filepath.Join(root, ".doppels", "runs", runID)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return nil, fmt.Errorf("create run state root: %w", err)
	}
	// Reserve the Run directory exclusively. Reusing a Run id must never
	// overwrite immutable records or append to another attempt's audit trail.
	if err := os.Mkdir(dir, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("run %s already exists", runID)
		}
		return nil, fmt.Errorf("create run state: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o700); err != nil {
		return nil, fmt.Errorf("create run state: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "artifacts"), 0o700); err != nil {
		return nil, fmt.Errorf("create artifact state: %w", err)
	}
	return &Store{dir: dir}, nil
}

func (s *Store) Dir() string { return s.dir }

func (s *Store) WriteRequest(value any) error { return s.writeJSON("request.json", value) }
func (s *Store) WriteRun(value any) error     { return s.writeJSON("run.json", value) }

func (s *Store) AppendEvent(value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	line, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode RunEvent: %w", err)
	}
	line = append(line, '\n')
	path := filepath.Join(s.dir, "events.jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open RunEvent log: %w", err)
	}
	defer file.Close()
	if err := writeAll(file, line); err != nil {
		return fmt.Errorf("append RunEvent: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync RunEvent: %w", err)
	}
	return nil
}

func (s *Store) WriteLog(stepID, stream string, data []byte) (string, error) {
	if !safeID.MatchString(stepID) {
		return "", fmt.Errorf("invalid step id %q", stepID)
	}
	if stream != "stdout" && stream != "stderr" {
		return "", fmt.Errorf("invalid log stream %q", stream)
	}
	relative := filepath.Join("logs", stepID+"."+stream+".log")
	if err := s.write(relative, data, 0o600); err != nil {
		return "", err
	}
	return relative, nil
}

// CopyArtifact snapshots a produced file into immutable Run state. The source
// has already been confined to the workspace by the execution package.
func (s *Store) CopyArtifact(id, filename, source string) (string, error) {
	if !safeID.MatchString(id) {
		return "", fmt.Errorf("invalid artifact id %q", id)
	}
	if filepath.Base(filename) != filename || filename == "." || filename == "" {
		return "", fmt.Errorf("invalid artifact filename %q", filename)
	}
	input, err := os.Open(source)
	if err != nil {
		return "", fmt.Errorf("open artifact: %w", err)
	}
	defer input.Close()

	relative := filepath.Join("artifacts", id+"-"+filename)
	target := filepath.Join(s.dir, relative)
	if err := atomicCopy(target, input, 0o600); err != nil {
		return "", fmt.Errorf("snapshot artifact: %w", err)
	}
	return target, nil
}

func (s *Store) writeJSON(name string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", name, err)
	}
	data = append(data, '\n')
	return s.write(name, data, 0o600)
}

func (s *Store) write(name string, data []byte, mode os.FileMode) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	if err := atomicWrite(path, data, mode); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".doppels-tmp-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if err := writeAll(temporary, data); err != nil {
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
	return os.Rename(temporaryName, path)
}

func atomicCopy(path string, source io.Reader, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".doppels-artifact-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	writer := bufio.NewWriter(temporary)
	if _, err := io.Copy(writer, source); err != nil {
		temporary.Close()
		return err
	}
	if err := writer.Flush(); err != nil {
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
	return os.Rename(temporaryName, path)
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return errors.New("short write")
		}
		data = data[written:]
	}
	return nil
}
