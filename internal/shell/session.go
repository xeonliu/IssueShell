//go:build darwin || linux

package shell

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Result struct {
	ExitCode int
	Status   string
	Duration time.Duration
}

type Session struct {
	shellPath string
	tempDir   string

	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	pending []byte

	waitOnce sync.Once
	waitErr  error
	waitCh   chan error
}

func Start(shellPath, tempDir string) (*Session, error) {
	if shellPath == "" {
		shellPath = os.Getenv("SHELL")
	}
	if shellPath == "" {
		shellPath = "/bin/sh"
	}
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		return nil, err
	}
	args := shellArgs(shellPath)
	cmd := exec.Command(shellPath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(), "PS1=", "PROMPT=")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	cmd.Stdout = writer
	cmd.Stderr = writer
	if err := cmd.Start(); err != nil {
		stdin.Close()
		reader.Close()
		writer.Close()
		return nil, fmt.Errorf("start shell %s: %w", shellPath, err)
	}
	writer.Close()
	session := &Session{
		shellPath: shellPath,
		tempDir:   tempDir,
		cmd:       cmd,
		stdin:     stdin,
		stdout:    reader,
		waitCh:    make(chan error, 1),
	}
	go func() {
		session.waitCh <- cmd.Wait()
		close(session.waitCh)
	}()
	// FD 3 is reserved for completion markers so ordinary stdout redirection cannot hide them.
	if _, err := io.WriteString(stdin, "exec 3>&1\n"); err != nil {
		session.Close()
		return nil, fmt.Errorf("initialize shell: %w", err)
	}
	return session, nil
}

func shellArgs(path string) []string {
	switch filepath.Base(path) {
	case "bash":
		return []string{"--noprofile", "--norc"}
	case "zsh":
		return []string{"-f"}
	default:
		return nil
	}
}

func (s *Session) Run(ctx context.Context, script string, output io.Writer) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	started := time.Now()
	if s.cmd == nil || s.cmd.Process == nil {
		return Result{}, errors.New("shell is closed")
	}
	file, err := os.CreateTemp(s.tempDir, "command-*.sh")
	if err != nil {
		return Result{}, err
	}
	path := file.Name()
	defer os.Remove(path)
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return Result{}, err
	}
	if _, err := io.WriteString(file, script); err != nil {
		file.Close()
		return Result{}, err
	}
	if err := file.Close(); err != nil {
		return Result{}, err
	}
	token, err := randomToken()
	if err != nil {
		return Result{}, err
	}
	marker := []byte("\x1eissueshell:" + token + ":")
	wrapper := fmt.Sprintf(". %s </dev/null\n__issueshell_rc=$?\nprintf '\\036issueshell:%s:%%s\\037' \"$__issueshell_rc\" >&3\n", quote(path), token)
	readDone := make(chan readResult, 1)
	go func() {
		exitCode, err := s.readUntilMarker(marker, output)
		readDone <- readResult{exitCode: exitCode, err: err}
	}()
	if _, err := io.WriteString(s.stdin, wrapper); err != nil {
		s.terminateLocked()
		<-readDone
		return s.exitedResult(started), fmt.Errorf("send command to shell: %w", err)
	}
	select {
	case read := <-readDone:
		if read.err == nil {
			return Result{ExitCode: read.exitCode, Status: "completed", Duration: time.Since(started)}, nil
		}
		if !errors.Is(read.err, io.EOF) {
			s.terminateLocked()
			return Result{ExitCode: read.exitCode, Status: "error", Duration: time.Since(started)}, read.err
		}
		return s.exitedResult(started), read.err
	case <-ctx.Done():
		s.terminateLocked()
		<-readDone
		return Result{ExitCode: 130, Status: "canceled", Duration: time.Since(started)}, ctx.Err()
	}
}

type readResult struct {
	exitCode int
	err      error
}

func (s *Session) readUntilMarker(marker []byte, output io.Writer) (int, error) {
	data := append([]byte(nil), s.pending...)
	s.pending = nil
	foundMarker := false
	var outputErr error
	buffer := make([]byte, 32*1024)
	for {
		if !foundMarker {
			if index := bytes.Index(data, marker); index >= 0 {
				if _, err := output.Write(data[:index]); err != nil && outputErr == nil {
					outputErr = err
				}
				data = data[index+len(marker):]
				foundMarker = true
			} else if len(data) >= len(marker) {
				flush := len(data) - len(marker) + 1
				if _, err := output.Write(data[:flush]); err != nil && outputErr == nil {
					outputErr = err
				}
				data = append(data[:0], data[flush:]...)
			}
		}
		if foundMarker {
			if end := bytes.IndexByte(data, 0x1f); end >= 0 {
				exitCode, err := strconv.Atoi(string(data[:end]))
				if err != nil {
					return 0, fmt.Errorf("invalid shell completion marker: %w", err)
				}
				s.pending = append(s.pending[:0], data[end+1:]...)
				if outputErr != nil {
					return exitCode, outputErr
				}
				return exitCode, nil
			}
		}
		n, err := s.stdout.Read(buffer)
		if n > 0 {
			data = append(data, buffer[:n]...)
		}
		if err != nil {
			if len(data) > 0 && !foundMarker {
				_, _ = output.Write(data)
			}
			return 0, err
		}
	}
}

func (s *Session) exitedResult(started time.Time) Result {
	err := s.wait()
	exitCode := 1
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		exitCode = exitErr.ExitCode()
	} else if err == nil {
		exitCode = 0
	}
	return Result{ExitCode: exitCode, Status: "shell_exited", Duration: time.Since(started)}
}

func (s *Session) wait() error {
	s.waitOnce.Do(func() { s.waitErr = <-s.waitCh })
	return s.waitErr
}

func (s *Session) terminateLocked() {
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_ = s.wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
	_ = s.stdin.Close()
	_ = s.stdout.Close()
	s.cmd = nil
}

func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.terminateLocked()
	return nil
}

func randomToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func quote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
