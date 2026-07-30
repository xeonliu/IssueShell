//go:build darwin || linux

package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/xeonliu/IssueShell/internal/githubapi"
	"github.com/xeonliu/IssueShell/internal/protocol"
	"github.com/xeonliu/IssueShell/internal/remote"
	shellproc "github.com/xeonliu/IssueShell/internal/shell"
	"github.com/xeonliu/IssueShell/internal/state"
)

type Config struct {
	API          *githubapi.Client
	Repo         string
	Issue        int
	Shell        string
	PollInterval time.Duration
	StateDir     string
	Output       io.Writer
	ErrorOutput  io.Writer
}

type execution struct {
	message    protocol.Message
	commentID  int64
	outputPath string
	cancel     context.CancelFunc
	done       chan executionResult
}

type executionResult struct {
	result shellproc.Result
	err    error
}

func Run(ctx context.Context, config Config) error {
	if config.API == nil {
		return errors.New("GitHub API client is required")
	}
	if config.Repo == "" || config.Issue <= 0 {
		return errors.New("repo and a positive issue number are required")
	}
	if config.StateDir == "" {
		return errors.New("state directory is required")
	}
	if err := githubapi.ValidateRepo(config.Repo); err != nil {
		return err
	}
	if config.PollInterval <= 0 {
		config.PollInterval = 5 * time.Second
	}
	if config.Output == nil {
		config.Output = os.Stdout
	}
	if config.ErrorOutput == nil {
		config.ErrorOutput = os.Stderr
	}
	repository, err := config.API.GetRepository(ctx, config.Repo)
	if err != nil {
		return err
	}
	if !repository.Private {
		return errors.New("IssueShell refuses to execute commands from a public repository")
	}
	issue, err := config.API.GetIssue(ctx, config.Repo, config.Issue)
	if err != nil {
		return err
	}
	sessionMessage, err := remote.SessionMessage(issue)
	if err != nil {
		return err
	}
	store, err := state.Open(config.StateDir)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := recoverInterrupted(ctx, config, store, sessionMessage.SessionID); err != nil {
		return err
	}
	cacheDir := filepath.Join(config.StateDir, "cache")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return err
	}
	session, err := shellproc.Start(config.Shell, cacheDir)
	if err != nil {
		return err
	}
	defer func() { _ = session.Close() }()
	fmt.Fprintf(config.ErrorOutput, "IssueShell client attached to %s#%d (session %s)\n", config.Repo, config.Issue, sessionMessage.SessionID)

	var current *execution
	closing := issue.State != "open"
	contextDone := ctx.Done()
	for {
		if closing {
			if current == nil {
				return nil
			}
			outcome := <-current.done
			if err := finishExecution(config, store, current, outcome, sessionMessage.SessionID); err != nil {
				return err
			}
			current = nil
			continue
		}
		issue, err = config.API.GetIssue(ctx, config.Repo, config.Issue)
		if err != nil {
			if ctx.Err() != nil {
				closing = true
				if current != nil {
					current.cancel()
				}
				continue
			}
			return err
		}
		if issue.State != "open" {
			closing = true
			if current != nil {
				current.cancel()
			}
			continue
		}
		comments, err := config.API.ListComments(ctx, config.Repo, config.Issue)
		if err != nil {
			if ctx.Err() != nil {
				closing = true
				if current != nil {
					current.cancel()
				}
				continue
			}
			return err
		}
		if hasSessionClose(comments, sessionMessage.SessionID) {
			closing = true
			if current != nil {
				current.cancel()
			}
			continue
		}
		if current != nil && hasCancel(comments, sessionMessage.SessionID, current.message.CommandID) {
			current.cancel()
		}
		if current == nil && !closing {
			current, err = startNext(config, store, session, sessionMessage.SessionID, comments, cacheDir)
			if err != nil {
				return err
			}
		}
		timer := time.NewTimer(config.PollInterval)
		select {
		case outcome := <-resultChannel(current):
			timer.Stop()
			if err := finishExecution(config, store, current, outcome, sessionMessage.SessionID); err != nil {
				return err
			}
			if outcome.result.Status != "completed" && !closing {
				session.Close()
				session, err = shellproc.Start(config.Shell, cacheDir)
				if err != nil {
					return fmt.Errorf("restart shell: %w", err)
				}
			}
			current = nil
		case <-timer.C:
		case <-contextDone:
			timer.Stop()
			closing = true
			contextDone = nil
			if current != nil {
				current.cancel()
			}
		}
	}
}

func resultChannel(current *execution) <-chan executionResult {
	if current == nil {
		return nil
	}
	return current.done
}

func startNext(config Config, store *state.Store, session *shellproc.Session, sessionID string, comments []githubapi.Comment, cacheDir string) (*execution, error) {
	for _, comment := range comments {
		message, payload, err := protocol.Decode(comment.Body)
		if err != nil || message.Kind != protocol.KindCommand || message.SessionID != sessionID || message.CommandID == "" {
			continue
		}
		if remote.HasDone(comments, sessionID, message.CommandID) {
			continue
		}
		exists, err := store.Exists(config.Repo, config.Issue, message.CommandID)
		if err != nil {
			return nil, err
		}
		if exists {
			continue
		}
		outputPath := filepath.Join(cacheDir, protocol.SHA256([]byte(message.CommandID))+".log")
		file, err := os.OpenFile(outputPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, fmt.Errorf("create command output cache: %w", err)
		}
		created, err := store.Begin(state.Record{
			Repo: config.Repo, Issue: config.Issue, CommandID: message.CommandID,
			CommentID: comment.ID, OutputPath: outputPath,
		})
		if err != nil {
			file.Close()
			_ = safeRemove(outputPath, config.StateDir)
			return nil, err
		}
		if !created {
			file.Close()
			_ = safeRemove(outputPath, config.StateDir)
			continue
		}
		runContext, cancel := context.WithCancel(context.Background())
		execution := &execution{
			message: message, commentID: comment.ID, outputPath: outputPath,
			cancel: cancel, done: make(chan executionResult, 1),
		}
		fmt.Fprintf(config.ErrorOutput, "Executing command %s\n", message.CommandID)
		go func() {
			result, runErr := session.Run(runContext, string(payload), io.MultiWriter(config.Output, file))
			if closeErr := file.Close(); runErr == nil && closeErr != nil {
				runErr = closeErr
			}
			execution.done <- executionResult{result: result, err: runErr}
		}()
		return execution, nil
	}
	return nil, nil
}

func finishExecution(config Config, store *state.Store, execution *execution, outcome executionResult, sessionID string) error {
	output, readErr := os.ReadFile(execution.outputPath)
	if readErr != nil {
		return fmt.Errorf("read command output cache: %w", readErr)
	}
	uploadContext, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if err := remote.UploadResult(uploadContext, config.API, config.Repo, config.Issue, sessionID, execution.message.CommandID,
		output, outcome.result.ExitCode, outcome.result.Status, outcome.result.Duration.Milliseconds()); err != nil {
		return err
	}
	status := state.StatusCompleted
	switch outcome.result.Status {
	case "canceled":
		status = state.StatusCanceled
	case "shell_exited", "error":
		status = state.StatusInterrupted
	}
	if err := store.SetStatus(config.Repo, config.Issue, execution.message.CommandID, status); err != nil {
		return err
	}
	if err := safeRemove(execution.outputPath, config.StateDir); err != nil {
		return err
	}
	if outcome.err != nil && outcome.result.Status != "canceled" && outcome.result.Status != "shell_exited" {
		fmt.Fprintf(config.ErrorOutput, "Command %s execution warning: %v\n", execution.message.CommandID, outcome.err)
	}
	fmt.Fprintf(config.ErrorOutput, "Command %s finished: %s (exit %d)\n", execution.message.CommandID, outcome.result.Status, outcome.result.ExitCode)
	return nil
}

func recoverInterrupted(ctx context.Context, config Config, store *state.Store, sessionID string) error {
	records, err := store.Running(config.Repo, config.Issue)
	if err != nil {
		return err
	}
	comments, err := config.API.ListComments(ctx, config.Repo, config.Issue)
	if err != nil {
		return err
	}
	for _, record := range records {
		if remote.HasDone(comments, sessionID, record.CommandID) {
			if err := store.SetStatus(config.Repo, config.Issue, record.CommandID, state.StatusCompleted); err != nil {
				return err
			}
			_ = safeRemove(record.OutputPath, config.StateDir)
			continue
		}
		outputPath, pathErr := checkedCachePath(record.OutputPath, config.StateDir)
		if pathErr != nil {
			return pathErr
		}
		output, readErr := os.ReadFile(outputPath)
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		}
		duration := time.Since(record.StartedAt).Milliseconds()
		if err := remote.UploadResult(ctx, config.API, config.Repo, config.Issue, sessionID, record.CommandID,
			output, 255, state.StatusInterrupted, duration); err != nil {
			return err
		}
		if err := store.SetStatus(config.Repo, config.Issue, record.CommandID, state.StatusInterrupted); err != nil {
			return err
		}
		_ = safeRemove(record.OutputPath, config.StateDir)
	}
	return nil
}

func hasCancel(comments []githubapi.Comment, sessionID, commandID string) bool {
	for _, comment := range comments {
		message, _, err := protocol.Decode(comment.Body)
		if err == nil && message.Kind == protocol.KindCancel && message.SessionID == sessionID && message.CommandID == commandID {
			return true
		}
	}
	return false
}

func hasSessionClose(comments []githubapi.Comment, sessionID string) bool {
	for _, comment := range comments {
		message, _, err := protocol.Decode(comment.Body)
		if err == nil && message.Kind == protocol.KindSessionClose && message.SessionID == sessionID {
			return true
		}
	}
	return false
}

func safeRemove(path, stateDir string) error {
	if path == "" {
		return nil
	}
	absPath, err := checkedCachePath(path, stateDir)
	if err != nil {
		return err
	}
	err = os.Remove(absPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func checkedCachePath(path, stateDir string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absState, err := filepath.Abs(stateDir)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(absState, absPath)
	if err != nil || relative == ".." || filepath.IsAbs(relative) || (len(relative) > 3 && relative[:3] == ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("cache path is outside state directory: %s", path)
	}
	return absPath, nil
}
