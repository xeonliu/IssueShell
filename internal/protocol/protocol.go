package protocol

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	Version = 1

	KindSession      = "session"
	KindCommand      = "command"
	KindCancel       = "cancel"
	KindSessionClose = "session_close"
	KindResultChunk  = "result_chunk"
	KindResultDone   = "result_done"

	markerPrefix = "<!-- issueshell:v1 "
	markerSuffix = " -->"
)

// Message is the machine-readable header attached to an Issue or comment.
type Message struct {
	Version       int    `json:"version"`
	Kind          string `json:"kind"`
	SessionID     string `json:"session_id"`
	CommandID     string `json:"command_id,omitempty"`
	ChunkIndex    int    `json:"chunk_index,omitempty"`
	ChunkCount    int    `json:"chunk_count,omitempty"`
	ExitCode      int    `json:"exit_code,omitempty"`
	Status        string `json:"status,omitempty"`
	DurationMS    int64  `json:"duration_ms,omitempty"`
	PayloadBytes  int    `json:"payload_bytes,omitempty"`
	PayloadSHA256 string `json:"payload_sha256,omitempty"`
}

func NewID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func SHA256(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// Encode renders a protocol message. Payload remains visible as a Markdown code block.
func Encode(message Message, payload []byte, language string) (string, error) {
	message.Version = Version
	message.PayloadBytes = len(payload)
	if len(payload) > 0 {
		message.PayloadSHA256 = SHA256(payload)
	}
	header, err := json.Marshal(message)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(header)
	var body strings.Builder
	body.WriteString(markerPrefix)
	body.WriteString(encoded)
	body.WriteString(markerSuffix)
	if len(payload) == 0 {
		return body.String(), nil
	}
	fence := strings.Repeat("`", max(3, longestRun(payload, '`')+1))
	body.WriteByte('\n')
	body.WriteString(fence)
	body.WriteString(language)
	body.WriteByte('\n')
	body.Write(payload)
	body.WriteByte('\n')
	body.WriteString(fence)
	return body.String(), nil
}

func Decode(body string) (Message, []byte, error) {
	var message Message
	firstLine, rest, _ := strings.Cut(body, "\n")
	if !strings.HasPrefix(firstLine, markerPrefix) || !strings.HasSuffix(firstLine, markerSuffix) {
		return message, nil, errors.New("not an IssueShell message")
	}
	rawHeader := strings.TrimSuffix(strings.TrimPrefix(firstLine, markerPrefix), markerSuffix)
	header, err := base64.RawURLEncoding.DecodeString(rawHeader)
	if err != nil {
		return message, nil, fmt.Errorf("decode header: %w", err)
	}
	if err := json.Unmarshal(header, &message); err != nil {
		return message, nil, fmt.Errorf("parse header: %w", err)
	}
	if message.Version != Version || message.Kind == "" || message.SessionID == "" {
		return message, nil, errors.New("invalid IssueShell header")
	}
	if message.PayloadBytes < 0 {
		return message, nil, errors.New("invalid negative IssueShell payload length")
	}
	if message.PayloadBytes == 0 {
		return message, nil, nil
	}
	_, framed, found := strings.Cut(rest, "\n")
	if !found || len(framed) < message.PayloadBytes {
		return message, nil, errors.New("truncated IssueShell payload")
	}
	payload := []byte(framed)[:message.PayloadBytes]
	if SHA256(payload) != message.PayloadSHA256 {
		return message, nil, errors.New("IssueShell payload hash mismatch")
	}
	return message, payload, nil
}

func longestRun(data []byte, target byte) int {
	longest, current := 0, 0
	for _, b := range data {
		if b == target {
			current++
			longest = max(longest, current)
		} else {
			current = 0
		}
	}
	return longest
}

// ChunkText splits valid UTF-8 without cutting a rune between comments.
func ChunkText(text string, maxBytes int) ([]string, error) {
	if maxBytes <= 0 {
		return nil, errors.New("maxBytes must be positive")
	}
	if !utf8.ValidString(text) {
		return nil, errors.New("text must be valid UTF-8")
	}
	if text == "" {
		return nil, nil
	}
	var chunks []string
	for len(text) > 0 {
		end := min(len(text), maxBytes)
		for end < len(text) && end > 0 && !utf8.RuneStart(text[end]) {
			end--
		}
		if end == 0 {
			_, size := utf8.DecodeRuneInString(text)
			if size > maxBytes {
				return nil, fmt.Errorf("maxBytes %s is smaller than one UTF-8 rune", strconv.Itoa(maxBytes))
			}
			end = size
		}
		chunks = append(chunks, text[:end])
		text = text[end:]
	}
	return chunks, nil
}

func DecodeUTF8(data []byte) string {
	return strings.ToValidUTF8(string(bytes.Clone(data)), "\uFFFD")
}
