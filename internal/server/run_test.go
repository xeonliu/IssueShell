package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xeonliu/IssueShell/internal/githubapi"
	"github.com/xeonliu/IssueShell/internal/protocol"
)

func TestSendRoundTripThroughGitHubComments(t *testing.T) {
	sessionID := "session-id"
	issueBody, err := protocol.Encode(protocol.Message{Kind: protocol.KindSession, SessionID: sessionID}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var comments []githubapi.Comment
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo":
			_ = json.NewEncoder(response).Encode(githubapi.Repository{Private: true})
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/issues/7":
			_ = json.NewEncoder(response).Encode(githubapi.Issue{Number: 7, State: "open", Body: issueBody})
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/issues/7/comments":
			mu.Lock()
			defer mu.Unlock()
			_ = json.NewEncoder(response).Encode(comments)
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/issues/7/comments":
			var input struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				t.Error(err)
				response.WriteHeader(http.StatusBadRequest)
				return
			}
			message, payload, err := protocol.Decode(input.Body)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			comments = append(comments, githubapi.Comment{ID: int64(len(comments) + 1), Body: input.Body})
			if message.Kind == protocol.KindCommand {
				if string(payload) != "printf remote" {
					t.Errorf("unexpected command: %q", payload)
				}
				chunk, _ := protocol.Encode(protocol.Message{
					Kind: protocol.KindResultChunk, SessionID: sessionID, CommandID: message.CommandID, ChunkCount: 1,
				}, []byte("remote"), "console")
				done, _ := protocol.Encode(protocol.Message{
					Kind: protocol.KindResultDone, SessionID: sessionID, CommandID: message.CommandID,
					ChunkCount: 1, ExitCode: 7, Status: "completed", PayloadSHA256: protocol.SHA256([]byte("remote")),
				}, nil, "")
				comments = append(comments,
					githubapi.Comment{ID: int64(len(comments) + 1), Body: chunk},
					githubapi.Comment{ID: int64(len(comments) + 2), Body: done})
			}
			created := comments[0]
			mu.Unlock()
			response.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(response).Encode(created)
		default:
			http.Error(response, fmt.Sprintf("unexpected %s %s", request.Method, request.URL.String()), http.StatusNotFound)
		}
	})
	api, err := githubapi.NewWithHTTPClient("secret", "https://github.test", memoryHTTPClient(handler))
	if err != nil {
		t.Fatal(err)
	}
	var output, diagnostics bytes.Buffer
	exitCode, err := Send(context.Background(), api, "owner/repo", 7, "printf remote", time.Millisecond, &output, &diagnostics, nil)
	if err != nil {
		t.Fatal(err)
	}
	if exitCode != 7 || output.String() != "remote" || !strings.Contains(diagnostics.String(), "waiting for client") {
		t.Fatalf("Send() = exit %d, output %q, diagnostics %q", exitCode, output.String(), diagnostics.String())
	}
}

func TestSendInterruptCancelsInFlightPollAndPostsCancellation(t *testing.T) {
	sessionID := "session-id"
	issueBody, err := protocol.Encode(protocol.Message{Kind: protocol.KindSession, SessionID: sessionID}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var comments []githubapi.Comment
	commandID := ""
	blockedPoll := false
	pollStarted := make(chan struct{})
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo":
			_ = json.NewEncoder(response).Encode(githubapi.Repository{Private: true})
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/issues/8":
			_ = json.NewEncoder(response).Encode(githubapi.Issue{Number: 8, State: "open", Body: issueBody})
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/issues/8/comments":
			mu.Lock()
			shouldBlock := commandID != "" && !blockedPoll
			if shouldBlock {
				blockedPoll = true
			}
			copyOfComments := append([]githubapi.Comment(nil), comments...)
			mu.Unlock()
			if shouldBlock {
				close(pollStarted)
				<-request.Context().Done()
				response.WriteHeader(499)
				return
			}
			_ = json.NewEncoder(response).Encode(copyOfComments)
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/issues/8/comments":
			var input struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				t.Error(err)
				return
			}
			message, _, err := protocol.Decode(input.Body)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			comments = append(comments, githubapi.Comment{ID: int64(len(comments) + 1), Body: input.Body})
			if message.Kind == protocol.KindCommand {
				commandID = message.CommandID
			}
			if message.Kind == protocol.KindCancel {
				done, _ := protocol.Encode(protocol.Message{
					Kind: protocol.KindResultDone, SessionID: sessionID, CommandID: message.CommandID,
					Status: "canceled", ExitCode: 130, PayloadSHA256: protocol.SHA256(nil),
				}, nil, "")
				comments = append(comments, githubapi.Comment{ID: int64(len(comments) + 1), Body: done})
			}
			created := comments[len(comments)-1]
			mu.Unlock()
			response.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(response).Encode(created)
		default:
			http.Error(response, "unexpected request", http.StatusNotFound)
		}
	})
	api, err := githubapi.NewWithHTTPClient("secret", "https://github.test", memoryHTTPClient(handler))
	if err != nil {
		t.Fatal(err)
	}
	interrupts := make(chan os.Signal, 1)
	go func() {
		<-pollStarted
		interrupts <- os.Interrupt
	}()
	var diagnostics bytes.Buffer
	exitCode, err := Send(context.Background(), api, "owner/repo", 8, "sleep 60", time.Hour, io.Discard, &diagnostics, interrupts)
	if err != nil {
		t.Fatal(err)
	}
	if exitCode != 130 || !strings.Contains(diagnostics.String(), "Cancellation requested") {
		t.Fatalf("Send() = exit %d, diagnostics %q", exitCode, diagnostics.String())
	}
	mu.Lock()
	defer mu.Unlock()
	foundCancel := false
	for _, comment := range comments {
		message, _, err := protocol.Decode(comment.Body)
		if err == nil && message.Kind == protocol.KindCancel && message.CommandID == commandID {
			foundCancel = true
		}
	}
	if !foundCancel {
		t.Fatal("cancellation comment was not posted")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func memoryHTTPClient(handler http.Handler) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		recorder := &responseRecorder{header: make(http.Header), status: http.StatusOK}
		handler.ServeHTTP(recorder, request)
		return &http.Response{
			StatusCode: recorder.status,
			Header:     recorder.header,
			Body:       io.NopCloser(bytes.NewReader(recorder.body.Bytes())),
			Request:    request,
		}, nil
	})}
}

type responseRecorder struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (recorder *responseRecorder) Header() http.Header { return recorder.header }
func (recorder *responseRecorder) Write(data []byte) (int, error) {
	return recorder.body.Write(data)
}
func (recorder *responseRecorder) WriteHeader(status int) { recorder.status = status }
