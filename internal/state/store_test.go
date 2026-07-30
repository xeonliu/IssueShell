package state

import (
	"path/filepath"
	"testing"
)

func TestStoreBeginAndRecover(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
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
}
