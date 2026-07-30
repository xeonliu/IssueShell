package githubapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const defaultBaseURL = "https://api.github.com"

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

type Repository struct {
	Private bool `json:"private"`
}

type User struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

type Issue struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	State   string `json:"state"`
	HTMLURL string `json:"html_url"`
	User    User   `json:"user"`
}

type Comment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	User      User      `json:"user"`
	CreatedAt time.Time `json:"created_at"`
}

type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("GitHub API returned %d: %s", e.StatusCode, e.Message)
}

func New(token, baseURL string) (*Client, error) {
	return NewWithHTTPClient(token, baseURL, &http.Client{Timeout: 30 * time.Second})
}

// NewWithHTTPClient allows callers to supply transport policy or an in-memory test transport.
func NewWithHTTPClient(token, baseURL string, httpClient *http.Client) (*Client, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("GITHUB_TOKEN is required")
	}
	if httpClient == nil {
		return nil, errors.New("HTTP client is required")
	}
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if _, err := url.ParseRequestURI(baseURL); err != nil {
		return nil, fmt.Errorf("invalid GitHub API URL: %w", err)
	}
	return &Client{
		baseURL: baseURL,
		token:   token,
		http:    httpClient,
	}, nil
}

func ValidateRepo(repo string) error {
	if !repositoryPattern.MatchString(repo) {
		return errors.New("repository must use OWNER/REPO format")
	}
	return nil
}

func (c *Client) GetRepository(ctx context.Context, repo string) (Repository, error) {
	var result Repository
	err := c.get(ctx, "/repos/"+repo, &result)
	return result, err
}

func (c *Client) GetIssue(ctx context.Context, repo string, number int) (Issue, error) {
	var result Issue
	err := c.get(ctx, fmt.Sprintf("/repos/%s/issues/%d", repo, number), &result)
	return result, err
}

func (c *Client) CreateIssue(ctx context.Context, repo, title, body string) (Issue, error) {
	var result Issue
	err := c.send(ctx, http.MethodPost, "/repos/"+repo+"/issues", map[string]string{"title": title, "body": body}, &result, false)
	return result, err
}

func (c *Client) SetIssueState(ctx context.Context, repo string, number int, state string) (Issue, error) {
	var result Issue
	err := c.send(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/issues/%d", repo, number), map[string]string{"state": state}, &result, true)
	return result, err
}

func (c *Client) ListComments(ctx context.Context, repo string, number int) ([]Comment, error) {
	var all []Comment
	for page := 1; ; page++ {
		var comments []Comment
		path := fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100&page=%d", repo, number, page)
		if err := c.get(ctx, path, &comments); err != nil {
			return nil, err
		}
		all = append(all, comments...)
		if len(comments) < 100 {
			return all, nil
		}
	}
}

func (c *Client) CreateComment(ctx context.Context, repo string, number int, body string) (Comment, error) {
	var result Comment
	err := c.send(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", repo, number), map[string]string{"body": body}, &result, false)
	return result, err
}

// CreateCommentUnique avoids duplicate protocol comments after an ambiguous POST failure.
func (c *Client) CreateCommentUnique(ctx context.Context, repo string, number int, body string, matches func(string) bool) (Comment, error) {
	find := func() (Comment, bool, error) {
		comments, err := c.ListComments(ctx, repo, number)
		if err != nil {
			return Comment{}, false, err
		}
		for _, comment := range comments {
			if matches(comment.Body) {
				return comment, true, nil
			}
		}
		return Comment{}, false, nil
	}
	existing, ok, err := find()
	if err != nil {
		return Comment{}, err
	}
	if ok {
		return existing, nil
	}
	created, err := c.CreateComment(ctx, repo, number, body)
	if err == nil {
		return created, nil
	}
	if existing, ok, findErr := find(); findErr == nil && ok {
		return existing, nil
	}
	return Comment{}, err
}

func (c *Client) get(ctx context.Context, path string, result any) error {
	return c.send(ctx, http.MethodGet, path, nil, result, true)
}

func (c *Client) send(ctx context.Context, method, path string, payload, result any, retry bool) error {
	var encoded []byte
	var err error
	if payload != nil {
		encoded, err = json.Marshal(payload)
		if err != nil {
			return err
		}
	}
	attempts := 1
	if retry {
		attempts = 4
	}
	for attempt := 0; attempt < attempts; attempt++ {
		var body io.Reader
		if encoded != nil {
			body = bytes.NewReader(encoded)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		req.Header.Set("User-Agent", "IssueShell/1")
		if encoded != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.http.Do(req)
		if err != nil {
			if retry && attempt+1 < attempts && ctx.Err() == nil {
				if err := sleepContext(ctx, time.Duration(1<<attempt)*time.Second); err != nil {
					return err
				}
				continue
			}
			return err
		}
		responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		resp.Body.Close()
		if readErr != nil {
			return readErr
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if result == nil || len(responseBody) == 0 {
				return nil
			}
			if err := json.Unmarshal(responseBody, result); err != nil {
				return fmt.Errorf("decode GitHub response: %w", err)
			}
			return nil
		}
		apiErr := &APIError{StatusCode: resp.StatusCode, Message: strings.TrimSpace(string(responseBody))}
		if !retry || attempt+1 == attempts || (resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusForbidden && resp.StatusCode < 500) {
			return apiErr
		}
		delay := retryDelay(resp, attempt)
		if err := sleepContext(ctx, delay); err != nil {
			return err
		}
	}
	return errors.New("GitHub request failed")
}

func retryDelay(response *http.Response, attempt int) time.Duration {
	if raw := response.Header.Get("Retry-After"); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds >= 0 {
			return time.Duration(seconds) * time.Second
		}
	}
	if raw := response.Header.Get("X-RateLimit-Reset"); raw != "" {
		if epoch, err := strconv.ParseInt(raw, 10, 64); err == nil {
			if wait := time.Until(time.Unix(epoch, 0)); wait > 0 {
				return wait
			}
		}
	}
	return time.Duration(1<<attempt) * time.Second
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
