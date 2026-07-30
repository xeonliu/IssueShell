package remote

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/xeonliu/IssueShell/internal/githubapi"
	"github.com/xeonliu/IssueShell/internal/protocol"
)

const MaxChunkBytes = 48 * 1024

type Result struct {
	CommandID  string
	Output     string
	ExitCode   int
	Status     string
	DurationMS int64
}

func SessionMessage(issue githubapi.Issue) (protocol.Message, error) {
	message, _, err := protocol.Decode(issue.Body)
	if err != nil {
		return protocol.Message{}, fmt.Errorf("issue #%d is not an IssueShell session: %w", issue.Number, err)
	}
	if message.Kind != protocol.KindSession {
		return protocol.Message{}, fmt.Errorf("issue #%d does not contain a session header", issue.Number)
	}
	return message, nil
}

func PostUnique(ctx context.Context, api *githubapi.Client, repo string, issue int, message protocol.Message, payload []byte, language, visible string) (githubapi.Comment, error) {
	body, err := protocol.Encode(message, payload, language)
	if err != nil {
		return githubapi.Comment{}, err
	}
	if visible != "" {
		body += "\n\n" + visible
	}
	return api.CreateCommentUnique(ctx, repo, issue, body, func(candidate string) bool {
		other, _, err := protocol.Decode(candidate)
		if err != nil {
			return false
		}
		return sameIdentity(message, other)
	})
}

func sameIdentity(a, b protocol.Message) bool {
	if a.Kind != b.Kind || a.SessionID != b.SessionID || a.CommandID != b.CommandID {
		return false
	}
	if a.Kind == protocol.KindResultChunk {
		return a.ChunkIndex == b.ChunkIndex
	}
	return true
}

func UploadResult(ctx context.Context, api *githubapi.Client, repo string, issue int, sessionID, commandID string, output []byte, exitCode int, status string, durationMS int64) error {
	text := protocol.DecodeUTF8(output)
	chunks, err := protocol.ChunkText(text, MaxChunkBytes)
	if err != nil {
		return err
	}
	for index, chunk := range chunks {
		message := protocol.Message{
			Kind:       protocol.KindResultChunk,
			SessionID:  sessionID,
			CommandID:  commandID,
			ChunkIndex: index,
			ChunkCount: len(chunks),
		}
		if _, err := PostUnique(ctx, api, repo, issue, message, []byte(chunk), "console", ""); err != nil {
			return fmt.Errorf("upload result chunk %d: %w", index+1, err)
		}
	}
	message := protocol.Message{
		Kind:          protocol.KindResultDone,
		SessionID:     sessionID,
		CommandID:     commandID,
		ChunkCount:    len(chunks),
		ExitCode:      exitCode,
		Status:        status,
		DurationMS:    durationMS,
		PayloadSHA256: protocol.SHA256([]byte(text)),
	}
	summary := fmt.Sprintf("IssueShell command `%s` finished with status **%s** and exit code `%d`.", commandID, status, exitCode)
	_, err = PostUnique(ctx, api, repo, issue, message, nil, "", summary)
	return err
}

// FindResult returns a result only after the done manifest and every chunk are present.
func FindResult(comments []githubapi.Comment, sessionID, commandID string) (Result, bool, error) {
	chunks := make(map[int][]byte)
	var done *protocol.Message
	for _, comment := range comments {
		message, payload, err := protocol.Decode(comment.Body)
		if err != nil || message.SessionID != sessionID || message.CommandID != commandID {
			continue
		}
		switch message.Kind {
		case protocol.KindResultChunk:
			if existing, ok := chunks[message.ChunkIndex]; ok && !strings.EqualFold(protocol.SHA256(existing), protocol.SHA256(payload)) {
				return Result{}, false, fmt.Errorf("conflicting result chunk %d", message.ChunkIndex)
			}
			chunks[message.ChunkIndex] = payload
		case protocol.KindResultDone:
			copy := message
			done = &copy
		}
	}
	if done == nil {
		return Result{}, false, nil
	}
	if done.ChunkCount != len(chunks) {
		return Result{}, false, nil
	}
	indexes := make([]int, 0, len(chunks))
	for index := range chunks {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	var output []byte
	for expected, index := range indexes {
		if index != expected {
			return Result{}, false, errors.New("result chunks are not contiguous")
		}
		output = append(output, chunks[index]...)
	}
	if protocol.SHA256(output) != done.PayloadSHA256 {
		return Result{}, false, errors.New("result manifest hash mismatch")
	}
	return Result{
		CommandID:  commandID,
		Output:     string(output),
		ExitCode:   done.ExitCode,
		Status:     done.Status,
		DurationMS: done.DurationMS,
	}, true, nil
}

func HasDone(comments []githubapi.Comment, sessionID, commandID string) bool {
	for _, comment := range comments {
		message, _, err := protocol.Decode(comment.Body)
		if err == nil && message.Kind == protocol.KindResultDone && message.SessionID == sessionID && message.CommandID == commandID {
			return true
		}
	}
	return false
}
