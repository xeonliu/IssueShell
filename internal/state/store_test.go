package state

import (
	"path/filepath"
	"testing"
)

func TestStoreBeginAndRecover(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "state")
	store, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	record := Record{Repo: "owner/repo", Issue: 3, CommandID: "cmd", CommentID: 9, OutputPath: "/tmp/output"}
	created, err := store.Begin(record)
	if err != nil || !created {
		t.Fatalf("Begin() = %v, %v", created, err)
	}
	created, err = store.Begin(record)
	if err != nil || created {
		t.Fatalf("duplicate Begin() = %v, %v", created, err)
	}
	running, err := store.Running(record.Repo, record.Issue)
	if err != nil || len(running) != 1 || running[0].CommandID != record.CommandID {
		t.Fatalf("Running() = %#v, %v", running, err)
	}
	if err := store.SetStatus(record.Repo, record.Issue, record.CommandID, StatusCompleted); err != nil {
		t.Fatal(err)
	}
	running, err = store.Running(record.Repo, record.Issue)
	if err != nil || len(running) != 0 {
		t.Fatalf("Running() after completion = %#v, %v", running, err)
	}
	if _, err := Open(directory); err == nil {
		t.Fatal("expected state directory lock error")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	exists, err := reopened.Exists(record.Repo, record.Issue, record.CommandID)
	if err != nil || !exists {
		t.Fatalf("persisted command exists = %v, %v", exists, err)
	}
}
