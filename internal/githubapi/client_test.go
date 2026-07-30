package githubapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestListCommentsPaginatesAndSetsHeaders(t *testing.T) {
	var requests atomic.Int32
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.Header.Get("Authorization") != "Bearer secret" || request.Header.Get("X-GitHub-Api-Version") == "" {
			t.Errorf("missing API headers: %#v", request.Header)
		}
		pageTwo := strings.Contains(request.URL.RawQuery, "page=2")
		count := 100
		if pageTwo {
			count = 1
		}
		comments := make([]Comment, count)
		for index := range comments {
			comments[index].ID = int64(index + 1)
		}
		_ = json.NewEncoder(response).Encode(comments)
	})
	client, err := NewWithHTTPClient("secret", "https://github.test", memoryHTTPClient(handler))
	if err != nil {
		t.Fatal(err)
	}
	comments, err := client.ListComments(context.Background(), "owner/repo", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 101 || requests.Load() != 2 {
		t.Fatalf("got %d comments with %d requests", len(comments), requests.Load())
	}
}

func TestCreateCommentUniqueUsesExistingComment(t *testing.T) {
	var posts atomic.Int32
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost {
			posts.Add(1)
		}
		_ = json.NewEncoder(response).Encode([]Comment{{ID: 42, Body: "existing"}})
	})
	client, err := NewWithHTTPClient("secret", "https://github.test", memoryHTTPClient(handler))
	if err != nil {
		t.Fatal(err)
	}
	comment, err := client.CreateCommentUnique(context.Background(), "owner/repo", 1, "new", func(body string) bool {
		return body == "existing"
	})
	if err != nil {
		t.Fatal(err)
	}
	if comment.ID != 42 || posts.Load() != 0 {
		t.Fatalf("comment = %#v, posts = %d", comment, posts.Load())
	}
}

func TestAPIErrorDoesNotExposeToken(t *testing.T) {
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Error(response, `{"message":"not found"}`, http.StatusNotFound)
	})
	client, err := NewWithHTTPClient("top-secret-token", "https://github.test", memoryHTTPClient(handler))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetIssue(context.Background(), "owner/repo", 1)
	if err == nil || strings.Contains(err.Error(), "top-secret-token") {
		t.Fatalf("unsafe error: %v", err)
	}
	var apiErr *APIError
	if !strings.Contains(fmt.Sprint(err), "404") || !strings.Contains(fmt.Sprint(err), "not found") || !asAPIError(err, &apiErr) {
		t.Fatalf("unexpected error: %v", err)
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

func asAPIError(err error, target **APIError) bool {
	value, ok := err.(*APIError)
	if ok {
		*target = value
	}
	return ok
}
