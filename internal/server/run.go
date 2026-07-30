package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
	"unicode/utf8"

	"github.com/xeonliu/IssueShell/internal/githubapi"
	"github.com/xeonliu/IssueShell/internal/protocol"
	"github.com/xeonliu/IssueShell/internal/remote"
)

const MaxCommandBytes = 48 * 1024

func Create(ctx context.Context, api *githubapi.Client, repo, title string) (githubapi.Issue, error) {
	if err := validatePrivate(ctx, api, repo); err != nil {
		return githubapi.Issue{}, err
	}
	sessionID, err := protocol.NewID()
	if err != nil {
		return githubapi.Issue{}, err
	}
	if title == "" {
		title = "IssueShell session " + time.Now().UTC().Format("2006-01-02 15:04:05 UTC")
	}
	header, err := protocol.Encode(protocol.Message{Kind: protocol.KindSession, SessionID: sessionID}, nil, "")
	if err != nil {
		return githubapi.Issue{}, err
	}
	body := header + "\n\nThis private Issue is an IssueShell command session. Closing it stops the attached client."
	return api.CreateIssue(ctx, repo, title, body)
}

// Send posts one command and waits for the matching result. The returned integer is the remote exit code.
func Send(ctx context.Context, api *githubapi.Client, repo string, issueNumber int, command string, pollInterval time.Duration, output, errorOutput io.Writer, interrupts <-chan os.Signal) (int, error) {
	if err := validatePrivate(ctx, api, repo); err != nil {
		return 255, err
	}
	if len(command) > MaxCommandBytes {
		return 255, fmt.Errorf("command is %d bytes; maximum is %d", len(command), MaxCommandBytes)
	}
	if !utf8.ValidString(command) {
		return 255, errors.New("command must be valid UTF-8")
	}
	if pollInterval <= 0 {
		pollInterval = 5 * time.Second
	}
	if output == nil {
		output = io.Discard
	}
	if errorOutput == nil {
		errorOutput = io.Discard
	}
	issue, session, err := loadOpenSession(ctx, api, repo, issueNumber)
	if err != nil {
		return 255, err
	}
	_ = issue
	commandID, err := protocol.NewID()
	if err != nil {
		return 255, err
	}
	message := protocol.Message{Kind: protocol.KindCommand, SessionID: session.SessionID, CommandID: commandID}
	if _, err := remote.PostUnique(ctx, api, repo, issueNumber, message, []byte(command), "sh", ""); err != nil {
		return 255, err
	}
	fmt.Fprintf(errorOutput, "Posted command %s; waiting for client\n", commandID)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	cancelPosted := false
	requestContext, cancelRequest := context.WithCancel(ctx)
	defer cancelRequest()
	watchDone := make(chan struct{})
	defer close(watchDone)
	if interrupts != nil {
		go func() {
			select {
			case <-interrupts:
				cancelRequest()
			case <-watchDone:
			case <-ctx.Done():
			}
		}()
	}
	requestCancellation := func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !cancelPosted {
			postContext, cancel := context.WithTimeout(context.Background(), time.Minute)
			err := postCancel(postContext, api, repo, issueNumber, session.SessionID, commandID)
			cancel()
			if err != nil {
				return err
			}
			cancelPosted = true
			fmt.Fprintf(errorOutput, "Cancellation requested for command %s; waiting for client\n", commandID)
		}
		requestContext = ctx
		return nil
	}
	for {
		comments, err := api.ListComments(requestContext, repo, issueNumber)
		if err != nil {
			if requestContext.Err() != nil {
				if err := requestCancellation(); err != nil {
					return 255, err
				}
				continue
			}
			return 255, err
		}
		result, found, err := remote.FindResult(comments, session.SessionID, commandID)
		if err != nil {
			return 255, err
		}
		if found {
			if _, err := io.WriteString(output, result.Output); err != nil {
				return 255, err
			}
			if result.Status == "canceled" {
				return 130, nil
			}
			return result.ExitCode, nil
		}
		select {
		case <-requestContext.Done():
			if err := requestCancellation(); err != nil {
				return 255, err
			}
		case <-ticker.C:
		}
	}
}

func Cancel(ctx context.Context, api *githubapi.Client, repo string, issueNumber int, commandID string) error {
	if commandID == "" {
		return errors.New("command-id is required")
	}
	if err := validatePrivate(ctx, api, repo); err != nil {
		return err
	}
	_, session, err := loadOpenSession(ctx, api, repo, issueNumber)
	if err != nil {
		return err
	}
	return postCancel(ctx, api, repo, issueNumber, session.SessionID, commandID)
}

func Close(ctx context.Context, api *githubapi.Client, repo string, issueNumber int) error {
	if err := validatePrivate(ctx, api, repo); err != nil {
		return err
	}
	issue, err := api.GetIssue(ctx, repo, issueNumber)
	if err != nil {
		return err
	}
	session, err := remote.SessionMessage(issue)
	if err != nil {
		return err
	}
	message := protocol.Message{Kind: protocol.KindSessionClose, SessionID: session.SessionID}
	if _, err := remote.PostUnique(ctx, api, repo, issueNumber, message, nil, "", "IssueShell session close requested."); err != nil {
		return err
	}
	_, err = api.SetIssueState(ctx, repo, issueNumber, "closed")
	return err
}

func postCancel(ctx context.Context, api *githubapi.Client, repo string, issueNumber int, sessionID, commandID string) error {
	message := protocol.Message{Kind: protocol.KindCancel, SessionID: sessionID, CommandID: commandID}
	_, err := remote.PostUnique(ctx, api, repo, issueNumber, message, nil, "", fmt.Sprintf("IssueShell cancellation requested for command `%s`.", commandID))
	return err
}

func validatePrivate(ctx context.Context, api *githubapi.Client, repo string) error {
	if api == nil {
		return errors.New("GitHub API client is required")
	}
	if err := githubapi.ValidateRepo(repo); err != nil {
		return err
	}
	repository, err := api.GetRepository(ctx, repo)
	if err != nil {
		return err
	}
	if !repository.Private {
		return errors.New("IssueShell requires a private repository")
	}
	return nil
}

func loadOpenSession(ctx context.Context, api *githubapi.Client, repo string, issueNumber int) (githubapi.Issue, protocol.Message, error) {
	if issueNumber <= 0 {
		return githubapi.Issue{}, protocol.Message{}, errors.New("a positive issue number is required")
	}
	issue, err := api.GetIssue(ctx, repo, issueNumber)
	if err != nil {
		return githubapi.Issue{}, protocol.Message{}, err
	}
	if issue.State != "open" {
		return githubapi.Issue{}, protocol.Message{}, fmt.Errorf("IssueShell session #%d is closed", issueNumber)
	}
	session, err := remote.SessionMessage(issue)
	return issue, session, err
}
