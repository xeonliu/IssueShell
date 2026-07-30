//go:build darwin || linux

package shell

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSessionPersistsDirectoryAndEnvironment(t *testing.T) {
	path := "/bin/sh"
	if _, err := os.Stat("/bin/bash"); err == nil {
		path = "/bin/bash"
	}
	session, err := Start(path, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	directory := t.TempDir()
	var output bytes.Buffer
	result, err := session.Run(context.Background(), "cd "+quote(directory)+"\nexport ISSUE_SHELL_TEST=preserved", &output)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("first command = %#v, %v, output %q", result, err, output.String())
	}
	output.Reset()
	result, err = session.Run(context.Background(), "pwd\nprintf '%s' \"$ISSUE_SHELL_TEST\"", &output)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("second command = %#v, %v", result, err)
	}
	if !strings.Contains(output.String(), filepath.Clean(directory)) || !strings.Contains(output.String(), "preserved") {
		t.Fatalf("state did not persist: %q", output.String())
	}
}

func TestSessionCancellation(t *testing.T) {
	session, err := Start("/bin/sh", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result, err := session.Run(ctx, "sleep 10", &bytes.Buffer{})
	if !errors.Is(err, context.DeadlineExceeded) || result.Status != "canceled" || result.ExitCode != 130 {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
}

func TestSessionRedirectionIsOrdinaryShellRedirection(t *testing.T) {
	session, err := Start("/bin/sh", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	destination := filepath.Join(t.TempDir(), "saved.txt")
	var output bytes.Buffer
	result, err := session.Run(context.Background(), "printf hidden > "+quote(destination), &output)
	if err != nil || result.ExitCode != 0 || output.Len() != 0 {
		t.Fatalf("Run() = %#v, %v, output %q", result, err, output.String())
	}
	saved, err := os.ReadFile(destination)
	if err != nil || string(saved) != "hidden" {
		t.Fatalf("saved output = %q, %v", saved, err)
	}
}
