package remote

import (
	"strings"
	"testing"

	"github.com/xeonliu/IssueShell/internal/githubapi"
	"github.com/xeonliu/IssueShell/internal/protocol"
)

func TestFindResultAssemblesOutOfOrderChunks(t *testing.T) {
	text := strings.Repeat("result-中文\n", 10000)
	chunks, err := protocol.ChunkText(text, MaxChunkBytes)
	if err != nil {
		t.Fatal(err)
	}
	var comments []githubapi.Comment
	for index := len(chunks) - 1; index >= 0; index-- {
		body, err := protocol.Encode(protocol.Message{
			Kind: protocol.KindResultChunk, SessionID: "s", CommandID: "c",
			ChunkIndex: index, ChunkCount: len(chunks),
		}, []byte(chunks[index]), "console")
		if err != nil {
			t.Fatal(err)
		}
		comments = append(comments, githubapi.Comment{Body: body})
	}
	done, err := protocol.Encode(protocol.Message{
		Kind: protocol.KindResultDone, SessionID: "s", CommandID: "c",
		ChunkCount: len(chunks), ExitCode: 7, Status: "completed", PayloadSHA256: protocol.SHA256([]byte(text)),
	}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	comments = append(comments, githubapi.Comment{Body: done})
	result, found, err := FindResult(comments, "s", "c")
	if err != nil || !found || result.Output != text || result.ExitCode != 7 {
		t.Fatalf("FindResult() = %#v, %v, %v", result, found, err)
	}
}
