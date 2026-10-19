package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// ---------------------------------------------------------------------------
// Ollama API client
// ---------------------------------------------------------------------------

// api carries the per-run connection settings threaded through API calls.
type api struct {
	Endpoint    string
	NumCtx      int
	Temperature float64
}

// chatRequest mirrors Ollama's /api/chat payload.
type chatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
	Options  reqOpts   `json:"options,omitempty"`
}

// reqOpts carries optional inference overrides; zero values are omitted from
// the payload, deferring to the model's saved defaults.
type reqOpts struct {
	NumCtx      int     `json:"num_ctx,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
}

// chatChunk is one line of a streamed response, or the whole non-streamed body.
type chatChunk struct {
	Message Message `json:"message"`
	Done    bool    `json:"done"`
	Error   string  `json:"error,omitempty"`
}

// buildRequest marshals the payload shared by streaming and complete calls.
func buildRequest(model string, msgs []Message, stream bool, a api) (io.Reader, error) {
	req := chatRequest{
		Model:    model,
		Messages: msgs,
		Stream:   stream,
		Options:  reqOpts{NumCtx: a.NumCtx, Temperature: a.Temperature},
	}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(data), nil
}

// streamChat POSTs with Stream:true and prints tokens as they arrive,
// returning the full accumulated reply for history storage. The server-side
// KV cache prefix is reused between calls while the model stays loaded,
// keeping incremental prefill cheap.
func streamChat(a api, model string, msgs []Message) (string, error) {
	body, err := buildRequest(model, msgs, true, a)
	if err != nil {
		return "", err
	}
	resp, err := http.Post(a.Endpoint, "application/json", body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)

	var full strings.Builder
	for sc.Scan() {
		var c chatChunk
		if json.Unmarshal(sc.Bytes(), &c) != nil {
			continue // tolerate malformed lines rather than die mid-reply
		}
		if c.Error != "" {
			return full.String(), fmt.Errorf("ollama: %s", c.Error)
		}
		fmt.Print(c.Message.Content)
		full.WriteString(c.Message.Content)
		if c.Done {
			break
		}
	}
	return full.String(), sc.Err()
}

// complete performs a non-streaming call, used for compaction summarization.
func complete(a api, model string, msgs []Message) (string, error) {
	body, err := buildRequest(model, msgs, false, a)
	if err != nil {
		return "", err
	}
	resp, err := http.Post(a.Endpoint, "application/json", body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var out chatChunk
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Error != "" {
		return "", fmt.Errorf("ollama: %s", out.Error)
	}
	return out.Message.Content, nil
}

// summarize merges evicted turns into the existing rolling summary so the
// persona retains early dialogue without paying verbatim-token cost. On
// failure it returns the previous summary unchanged (never data loss).
func summarize(a api, model string, evicted []Message, prev string) string {
	const sys = "You are a conversation archivist. Merge the conversation excerpt into the previous summary, if any. Preserve personal facts, decisions, names, commitments, and unresolved questions. Write in third person, at most ~400 words. Output only the summary."

	var b strings.Builder
	if prev != "" {
		b.WriteString("Previous summary:\n")
		b.WriteString(prev)
		b.WriteString("\n\n")
	}
	b.WriteString("Conversation excerpt:\n")
	for _, m := range evicted {
		fmt.Fprintf(&b, "%s: %s\n", m.Role, m.Content)
	}
	b.WriteString("\nUpdated summary:")

	text, err := complete(a, model, []Message{
		{Role: "system", Content: sys},
		{Role: "user", Content: b.String()},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n[warn] compaction failed: %v (keeping previous summary)\n", err)
		return prev
	}
	return text
}