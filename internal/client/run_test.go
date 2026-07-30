//go:build darwin || linux

package client

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/xeonliu/IssueShell/internal/githubapi"
	"github.com/xeonliu/IssueShell/internal/protocol"
	"github.com/xeonliu/IssueShell/internal/remote"
)

func TestRunExecutesCommandUploadsResultAndStopsOnClose(t *testing.T) {
	sessionID := "test-session"
	commandID := "test-command"
	issueBody, err := protocol.Encode(protocol.Message{Kind: protocol.KindSession, SessionID: sessionID}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	commandBody, err := protocol.Encode(protocol.Message{
		Kind: protocol.KindCommand, SessionID: sessionID, CommandID: commandID,
	}, []byte("printf client-result"), "sh")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	comments := []githubapi.Comment{{ID: 1, Body: commandBody}}
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo":
			_ = json.NewEncoder(response).Encode(githubapi.Repository{Private: true})
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/issues/9":
			state := "open"
			if remote.HasDone(comments, sessionID, commandID) {
				state = "closed"
			}
			_ = json.NewEncoder(response).Encode(githubapi.Issue{Number: 9, State: state, Body: issueBody})
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/issues/9/comments":
			_ = json.NewEncoder(response).Encode(comments)
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/issues/9/comments":
			var input struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				t.Error(err)
				response.WriteHeader(http.StatusBadRequest)
				return
			}
			comment := githubapi.Comment{ID: int64(len(comments) + 1), Body: input.Body}
			comments = append(comments, comment)
			response.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(response).Encode(comment)
		default:
			http.Error(response, "unexpected request", http.StatusNotFound)
		}
	})
	api, err := githubapi.NewWithHTTPClient("secret", "https://github.test", memoryHTTPClient(handler))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var output, diagnostics bytes.Buffer
	err = Run(ctx, Config{
		API: api, Repo: "owner/repo", Issue: 9, Shell: "/bin/sh",
		PollInterval: time.Millisecond, StateDir: t.TempDir(), Output: &output, ErrorOutput: &diagnostics,
	})
	if err != nil {
		t.Fatal(err)
	}
	if output.String() != "client-result" {
		t.Fatalf("local output = %q", output.String())
	}
	mu.Lock()
	result, found, err := remote.FindResult(comments, sessionID, commandID)
	mu.Unlock()
	if err != nil || !found || result.Output != "client-result" || result.ExitCode != 0 {
		t.Fatalf("remote result = %#v, found %v, err %v", result, found, err)
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
