package protocol

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestEncodeDecodePayload(t *testing.T) {
	payload := []byte("first\n```\n中文\nlast")
	want := Message{Kind: KindCommand, SessionID: "session", CommandID: "command"}
	body, err := Encode(want, payload, "sh")
	if err != nil {
		t.Fatal(err)
	}
	got, gotPayload, err := Decode(body)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != want.Kind || got.CommandID != want.CommandID || string(gotPayload) != string(payload) {
		t.Fatalf("round trip mismatch: %#v %q", got, gotPayload)
	}
}

func TestDecodeRejectsNegativePayloadLength(t *testing.T) {
	header, err := json.Marshal(Message{Version: Version, Kind: KindCommand, SessionID: "s", PayloadBytes: -1})
	if err != nil {
		t.Fatal(err)
	}
	body := markerPrefix + base64.RawURLEncoding.EncodeToString(header) + markerSuffix
	if _, _, err := Decode(body); err == nil {
		t.Fatal("expected negative length error")
	}
}

func TestDecodeRejectsChangedPayload(t *testing.T) {
	body, err := Encode(Message{Kind: KindCommand, SessionID: "s"}, []byte("hello"), "sh")
	if err != nil {
		t.Fatal(err)
	}
	body = strings.Replace(body, "hello", "jello", 1)
	if _, _, err := Decode(body); err == nil {
		t.Fatal("expected hash error")
	}
}

func TestChunkTextPreservesUTF8(t *testing.T) {
	want := strings.Repeat("甲a", 50)
	chunks, err := ChunkText(want, 7)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(chunks, "") != want {
		t.Fatal("chunks did not reconstruct text")
	}
	for _, chunk := range chunks {
		if len(chunk) > 7 {
			t.Fatalf("chunk too large: %d", len(chunk))
		}
	}
}
