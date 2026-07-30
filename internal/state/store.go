package state

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const (
	StatusRunning     = "running"
	StatusCompleted   = "completed"
	StatusCanceled    = "canceled"
	StatusInterrupted = "interrupted"
)

type Record struct {
	Repo       string
	Issue      int
	CommandID  string
	CommentID  int64
	Status     string
	OutputPath string
	StartedAt  time.Time
}

type Store struct {
	db *sql.DB
}

func Open(directory string) (*Store, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	path := filepath.Join(directory, "state.sqlite3")
	dsn := (&url.URL{Scheme: "file", Path: path}).String() + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open state database: %w", err)
	}
	_ = os.Chmod(path, 0o600)
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS commands (
			repo TEXT NOT NULL,
			issue_number INTEGER NOT NULL,
			command_id TEXT NOT NULL,
			comment_id INTEGER NOT NULL,
			status TEXT NOT NULL,
			output_path TEXT NOT NULL,
			started_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY (repo, issue_number, command_id)
		)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize state database: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Begin records a command before execution. false means the command was already seen.
func (s *Store) Begin(record Record) (bool, error) {
	now := time.Now().UTC()
	if record.StartedAt.IsZero() {
		record.StartedAt = now
	}
	result, err := s.db.Exec(`
		INSERT INTO commands (repo, issue_number, command_id, comment_id, status, output_path, started_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(repo, issue_number, command_id) DO NOTHING`,
		record.Repo, record.Issue, record.CommandID, record.CommentID, StatusRunning,
		record.OutputPath, record.StartedAt.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func (s *Store) SetStatus(repo string, issue int, commandID, status string) error {
	result, err := s.db.Exec(`
		UPDATE commands SET status = ?, updated_at = ?
		WHERE repo = ? AND issue_number = ? AND command_id = ?`,
		status, time.Now().UTC().Format(time.RFC3339Nano), repo, issue, commandID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return errors.New("command state not found")
	}
	return nil
}

func (s *Store) Exists(repo string, issue int, commandID string) (bool, error) {
	var one int
	err := s.db.QueryRow(`
		SELECT 1 FROM commands WHERE repo = ? AND issue_number = ? AND command_id = ?`,
		repo, issue, commandID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) Running(repo string, issue int) ([]Record, error) {
	rows, err := s.db.Query(`
		SELECT repo, issue_number, command_id, comment_id, status, output_path, started_at
		FROM commands WHERE repo = ? AND issue_number = ? AND status = ?
		ORDER BY started_at`, repo, issue, StatusRunning)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []Record
	for rows.Next() {
		var record Record
		var started string
		if err := rows.Scan(&record.Repo, &record.Issue, &record.CommandID, &record.CommentID, &record.Status, &record.OutputPath, &started); err != nil {
			return nil, err
		}
		record.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
		records = append(records, record)
	}
	return records, rows.Err()
}
