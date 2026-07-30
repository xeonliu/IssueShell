//go:build darwin || linux

package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const (
	StatusRunning     = "running"
	StatusCompleted   = "completed"
	StatusCanceled    = "canceled"
	StatusInterrupted = "interrupted"

	stateVersion = 1
)

type Record struct {
	Repo       string    `json:"repo"`
	Issue      int       `json:"issue"`
	CommandID  string    `json:"command_id"`
	CommentID  int64     `json:"comment_id"`
	Status     string    `json:"status"`
	OutputPath string    `json:"output_path"`
	StartedAt  time.Time `json:"started_at"`
}

type diskState struct {
	Version  int               `json:"version"`
	Commands map[string]Record `json:"commands"`
}

type Store struct {
	mu       sync.Mutex
	path     string
	lockFile *os.File
	state    diskState
}

func Open(directory string) (*Store, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	lockPath := filepath.Join(directory, "state.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open state lock: %w", err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lockFile.Close()
		return nil, errors.New("state directory is already in use by another IssueShell client")
	}
	store := &Store{
		path:     filepath.Join(directory, "state.json"),
		lockFile: lockFile,
		state: diskState{
			Version:  stateVersion,
			Commands: make(map[string]Record),
		},
	}
	data, err := os.ReadFile(store.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		store.Close()
		return nil, fmt.Errorf("read state file: %w", err)
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &store.state); err != nil {
			store.Close()
			return nil, fmt.Errorf("parse state file: %w", err)
		}
		if store.state.Version != stateVersion || store.state.Commands == nil {
			store.Close()
			return nil, fmt.Errorf("unsupported IssueShell state version %d", store.state.Version)
		}
	}
	return store, nil
}

func (s *Store) Close() error {
	if s.lockFile == nil {
		return nil
	}
	err := syscall.Flock(int(s.lockFile.Fd()), syscall.LOCK_UN)
	closeErr := s.lockFile.Close()
	s.lockFile = nil
	if err != nil {
		return err
	}
	return closeErr
}

// Begin records a command before execution. false means the command was already seen.
func (s *Store) Begin(record Record) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := recordKey(record.Repo, record.Issue, record.CommandID)
	if _, exists := s.state.Commands[key]; exists {
		return false, nil
	}
	if record.StartedAt.IsZero() {
		record.StartedAt = time.Now().UTC()
	}
	record.Status = StatusRunning
	s.state.Commands[key] = record
	if err := s.persist(); err != nil {
		delete(s.state.Commands, key)
		return false, err
	}
	return true, nil
}

func (s *Store) SetStatus(repo string, issue int, commandID, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := recordKey(repo, issue, commandID)
	record, exists := s.state.Commands[key]
	if !exists {
		return errors.New("command state not found")
	}
	previous := record
	record.Status = status
	s.state.Commands[key] = record
	if err := s.persist(); err != nil {
		s.state.Commands[key] = previous
		return err
	}
	return nil
}

func (s *Store) Exists(repo string, issue int, commandID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, exists := s.state.Commands[recordKey(repo, issue, commandID)]
	return exists, nil
}

func (s *Store) Running(repo string, issue int) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var records []Record
	for _, record := range s.state.Commands {
		if record.Repo == repo && record.Issue == issue && record.Status == StatusRunning {
			records = append(records, record)
		}
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].StartedAt.Before(records[j].StartedAt)
	})
	return records, nil
}

func (s *Store) persist() error {
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	directory := filepath.Dir(s.path)
	temporary, err := os.CreateTemp(directory, ".state-*.tmp")
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
	if err := os.Rename(temporaryPath, s.path); err != nil {
		return err
	}
	directoryHandle, err := os.Open(directory)
	if err != nil {
		return err
	}
	err = directoryHandle.Sync()
	closeErr := directoryHandle.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func recordKey(repo string, issue int, commandID string) string {
	return repo + "\x00" + strconv.Itoa(issue) + "\x00" + commandID
}
