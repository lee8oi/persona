// Package main implements "persona": a minimal, stdlib-only Ollama chat
// wrapper supporting multiple personas defined as small text files, with
// persistent per-persona sessions, streaming replies, token-budget
// trimming, and optional summarization compaction.
//
// Layout:
//
//	~/.config/persona/personas/*.md              persona definitions
//	~/.local/share/persona/<name>/sessions/*.json per-persona history
//
// Usage:
//
//	persona                          show help and list personas
//	persona list                     list personas with session counts
//	persona init <name>              create a starter persona file
//	persona <name>                   resume the latest session
//	persona <name> -new              start a fresh session
//	persona <name> -resume <file>    resume a specific session file
//	persona <name> -model <tag>      override the persona's model
//
// Session files may be resumed by absolute path, cwd-relative path, or bare
// filename (resolved against the persona's sessions directory).
//
// REPL commands: /help /stats /summary /model <name> /save <name> /new /quit
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// Message is a single chat turn in Ollama's /api/chat format.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Session is the on-disk unit of persistence per persona: everything needed
// to reconstruct a conversation, including the persona system prompt.
type Session struct {
	Persona  string    `json:"persona,omitempty"` // owning persona name
	Model    string    `json:"model"`             // model tag used for this session
	System   string    `json:"system"`            // persona system prompt
	Summary  string    `json:"summary,omitempty"` // rolling compaction of evicted turns
	Messages []Message `json:"messages"`          // live dialogue (user/assistant pairs)
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

// Persona is a named system prompt plus inference defaults, loaded from a
// small file with "key: value" front matter above the system prompt.
type Persona struct {
	Name        string
	Model       string
	System      string
	Temperature float64 // 0 = unset, use server default (limits exact-0 tuning)
}

// api carries the per-run connection settings threaded through API calls.
type api struct {
	Endpoint    string
	NumCtx      int
	Temperature float64
}

// ---------------------------------------------------------------------------
// Persona loading
// ---------------------------------------------------------------------------

// loadPersona parses a persona file: optional front matter delimited by
// "---" lines, followed by the system prompt as free text.
func loadPersona(path string) (*Persona, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	content := string(data)

	header, system := "", strings.TrimSpace(content)
	if strings.HasPrefix(content, "---\n") || strings.HasPrefix(content, "---\r\n") {
		rest := strings.TrimPrefix(strings.TrimPrefix(content, "---\r\n"), "---\n")
		idx := strings.Index(rest, "\n---")
		if idx == -1 {
			return nil, fmt.Errorf("unterminated front matter (missing closing ---)")
		}
		header = rest[:idx]
		system = strings.TrimSpace(rest[idx+len("\n---"):])
	}

	p := &Persona{}
	for _, line := range strings.Split(header, "\n") {
		line = strings.TrimSuffix(line, "\r")
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		switch key {
		case "name":
			p.Name = val
		case "model":
			p.Model = val
		case "temperature":
			t, perr := strconv.ParseFloat(val, 64)
			if perr != nil {
				return nil, fmt.Errorf("bad temperature %q: %w", val, perr)
			}
			p.Temperature = t
		}
	}
	if p.Name == "" {
		base := filepath.Base(path)
		p.Name = strings.TrimSuffix(base, filepath.Ext(base))
	}
	if p.Model == "" {
		return nil, fmt.Errorf("persona must set 'model:' in front matter")
	}
	p.System = system
	return p, nil
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

// Save writes the session as pretty-printed JSON, creating directories as needed.
func (s *Session) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// LoadSession reads a previously saved session file.
func LoadSession(path string) (*Session, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// findLatestSession returns the newest session file in dir, assuming the
// timestamp-based filenames (2006-01-02-150405.json) used by this program,
// which sort lexicographically in chronological order.
func findLatestSession(dir string) (string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	latest := ""
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		if name > latest {
			latest = name
		}
	}
	if latest == "" {
		return "", false
	}
	return filepath.Join(dir, latest), true
}

// resolveSessionPath finds a session file by name or path. Absolute paths
// pass through; bare names and relative paths are looked up inside the
// persona's sessions directory.
func resolveSessionPath(arg, dataDir string) string {
	if arg == "" {
		return arg
	}
	if _, err := os.Stat(arg); err == nil {
		return arg // exists as given (absolute or cwd-relative)
	}
	candidate := filepath.Join(dataDir, arg)
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return arg // neither found; let LoadSession surface the error
}

// ---------------------------------------------------------------------------
// Prompt assembly and token budgeting
// ---------------------------------------------------------------------------

// estimateTokens approximates token count at ~4 characters per token.
// Deliberately conservative; tuned for English prose.
func estimateTokens(s string) int {
	return utf8.RuneCountInString(s)/4 + 1
}

// promptEstimate totals the rough token cost of a message list.
func promptEstimate(msgs []Message) int {
	total := 16 // per-message framing overhead
	for _, m := range msgs {
		total += estimateTokens(m.Content) + 4
	}
	return total
}

// buildPrompt assembles what actually gets sent: persona system prompt,
// rolling summary (if any), then live history. The summary is injected at
// request time rather than stored inside Messages, so it never interferes
// with turn-pair trimming.
func (s *Session) buildPrompt() []Message {
	out := make([]Message, 0, len(s.Messages)+2)
	out = append(out, Message{Role: "system", Content: s.System})
	if s.Summary != "" {
		out = append(out, Message{
			Role:    "system",
			Content: "Summary of earlier parts of this conversation:\n" + s.Summary,
		})
	}
	return append(out, s.Messages...)
}

// trim evicts oldest exchanges until the assembled prompt fits the budget,
// returning the evicted messages for optional compaction. The persona
// system prompt is never evicted because it is rebuilt in buildPrompt;
// only dialogue turns are touched here.
func (s *Session) trim(budget int) []Message {
	var evicted []Message
	for promptEstimate(s.buildPrompt()) > budget && len(s.Messages) >= 2 {
		// Evict a full user+assistant pair; a leading lone assistant
		// message (only possible in odd hand-edited files) goes singly.
		n := 2
		if s.Messages[0].Role == "assistant" {
			n = 1
		}
		evicted = append(evicted, s.Messages[:n]...)
		s.Messages = s.Messages[n:]
	}
	return evicted
}

// ---------------------------------------------------------------------------
// Ollama API
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Subcommands
// ---------------------------------------------------------------------------

// listCommand prints every persona with its model and session count.
func listCommand(personasDir, dataRoot string) {
	entries, err := os.ReadDir(personasDir)
	if err != nil {
		fmt.Printf("(no personas yet — create one with: persona init <name>)\n")
		return
	}
	for _, e := range entries {
		if e.IsDir() || (!strings.HasSuffix(e.Name(), ".md") && !strings.HasSuffix(e.Name(), ".txt")) {
			continue
		}
		p, perr := loadPersona(filepath.Join(personasDir, e.Name()))
		if perr != nil {
			fmt.Fprintf(os.Stderr, "skipping %s: %v\n", e.Name(), perr)
			continue
		}
		sdir := filepath.Join(dataRoot, p.Name, "sessions")
		n, latest := 0, ""
		if ents, serr := os.ReadDir(sdir); serr == nil {
			for _, se := range ents {
				if !se.IsDir() && strings.HasSuffix(se.Name(), ".json") {
					n++
					if se.Name() > latest {
						latest = se.Name()
					}
				}
			}
		}
		fmt.Printf("  %-16s model=%-18s sessions=%d", p.Name, p.Model, n)
		if latest != "" {
			fmt.Printf("  latest=%s", latest)
		}
		fmt.Println()
	}
}

// initCommand writes a starter persona file.
func initCommand(name, personasDir string) {
	path := filepath.Join(personasDir, name+".md")
	if _, err := os.Stat(path); err == nil {
		fmt.Fprintf(os.Stderr, "already exists: %s\n", path)
		os.Exit(1)
	}
	if err := os.MkdirAll(personasDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	tmpl := "---\nname: " + name + "\nmodel: qwen3-14b-32k\ntemperature: 0.8\n---\n\n" +
		"You are ... Describe this persona's identity, voice, knowledge, and rules here.\n"
	if err := os.WriteFile(path, []byte(tmpl), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("created: %s\n", path)
	fmt.Printf("next: edit the file (system prompt + front matter), then run 'persona %s'\n", name)
}

// printUsage shows top-level usage and delegates to listCommand.
func printUsage(personasDir, dataRoot string) {
	fmt.Printf(`persona - multi-persona Ollama chat wrapper

usage:
  persona                          show this help and list personas
  persona list                     list personas and session counts
  persona init <name>              create a starter persona file
  persona <name>                   resume the latest session
  persona <name> -new              start a fresh session
  persona <name> -resume <file>    resume a specific session file
                                     (absolute path, cwd-relative path,
                                      or bare filename resolved against
                                      the persona's sessions dir)
  persona <name> -model <tag>      override the persona's model

personas:  %s
sessions:  %s

personas:
`, personasDir, dataRoot)
	listCommand(personasDir, dataRoot)
}

// ---------------------------------------------------------------------------
// REPL
// ---------------------------------------------------------------------------

func main() {
	home, _ := os.UserHomeDir()
	personasDir := filepath.Join(home, ".config", "persona", "personas")
	dataRoot := filepath.Join(home, ".local", "share", "persona")

	args := os.Args[1:]
	if len(args) == 0 {
		printUsage(personasDir, dataRoot)
		return
	}
	switch args[0] {
	case "help", "-h", "--help":
		printUsage(personasDir, dataRoot)
		return
	case "list":
		listCommand(personasDir, dataRoot)
		return
	case "init":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: persona init <name>")
			os.Exit(1)
		}
		initCommand(args[1], personasDir)
		return
	}

	personaName := args[0]
	fs := flag.NewFlagSet("persona "+personaName, flag.ExitOnError)
	endpoint := fs.String("endpoint", "http://localhost:11434/api/chat", "Ollama chat endpoint")
	newSess := fs.Bool("new", false, "start a new session instead of resuming the latest")
	resume := fs.String("resume", "", "resume this session file instead of the latest")
	modelFlag := fs.String("model", "", "override the persona's model for this run")
	budget := fs.Int("budget", 16000, "approximate token budget for live history")
	ctxSize := fs.Int("ctx", 0, "override num_ctx (0 = model default)")
	compact := fs.Bool("compact", false, "summarize evicted turns instead of dropping them")
	fs.Parse(args[1:])

	p, err := loadPersona(filepath.Join(personasDir, personaName+".md"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "persona %q: %v\n", personaName, err)
		fmt.Fprintf(os.Stderr, "(create it with: persona init %s)\n", personaName)
		os.Exit(1)
	}

	a := api{Endpoint: *endpoint, NumCtx: *ctxSize, Temperature: p.Temperature}
	model := p.Model
	if *modelFlag != "" {
		model = *modelFlag
	}

	dataDir := filepath.Join(dataRoot, p.Name, "sessions")

	var sess *Session
	var sessionPath string
	resumed := false

	if *resume != "" {
		sess, err = LoadSession(resolveSessionPath(*resume, dataDir))
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to load session: %v\n", err)
			os.Exit(1)
		}
		sessionPath = resolveSessionPath(*resume, dataDir)
		resumed = true
	} else if !*newSess {
		if latest, ok := findLatestSession(dataDir); ok {
			sess, err = LoadSession(latest)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to load latest session: %v\n", err)
				os.Exit(1)
			}
			sessionPath = latest
			resumed = true
		}
	}
	if sess == nil {
		sess = &Session{Persona: p.Name, Model: model, System: p.System}
		sessionPath = filepath.Join(dataDir, time.Now().Format("2006-01-02-150405")+".json")
	}
	if *modelFlag == "" && sess.Model != "" {
		model = sess.Model // honor the session's recorded model unless overridden
	}

	if resumed {
		fmt.Printf("persona %s, resuming %s (%d turns)\n", p.Name, filepath.Base(sessionPath), len(sess.Messages)/2)
	} else {
		fmt.Printf("persona %s, new session\n", p.Name)
	}
	fmt.Printf("model: %s   budget: ~%d tokens   compact: %v   (/help for commands)\n",
		model, *budget, *compact)

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1024*1024), 8*1024*1024) // allow large pastes

	running := true
	for running {
		fmt.Print("\nyou> ")
		if !in.Scan() {
			break
		}
		line := strings.TrimSpace(in.Text())

		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "/") {
			running = !handleCommand(line, &sess, &sessionPath, &model, budget, dataDir)
			continue
		}

		sess.Messages = append(sess.Messages, Message{Role: "user", Content: line})

		evicted := sess.trim(*budget)
		if len(evicted) > 0 {
			if *compact {
				sess.Summary = summarize(a, model, evicted, sess.Summary)
			}
			fmt.Printf("[context trimmed: %d turns evicted]\n", len(evicted))
		}

		fmt.Printf("\n%s> ", p.Name)
		reply, err := streamChat(a, model, sess.buildPrompt())
		fmt.Println()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[error] %v (turn discarded, retry)\n", err)
			sess.Messages = sess.Messages[:len(sess.Messages)-1]
			continue
		}
		sess.Messages = append(sess.Messages, Message{Role: "assistant", Content: reply})
		sess.Save(sessionPath) // crash-safe: persist after every exchange
	}

	if err := sess.Save(sessionPath); err != nil {
		fmt.Fprintf(os.Stderr, "[error] final save failed: %v\n", err)
	}
	fmt.Printf("\nsession saved: %s\n", sessionPath)
}

// handleCommand processes slash commands; returns true when the loop should
// exit. It receives a **Session so that /new can rebind the caller's
// session pointer, and *string for model so that /model can update it.
func handleCommand(line string, sessPtr **Session, path *string, model *string, budget *int, dir string) bool {
	cmd, arg, _ := strings.Cut(strings.TrimPrefix(line, "/"), " ")
	sess := *sessPtr

	switch cmd {

	case "help":
		fmt.Println(`/stats               session statistics
/summary             show current compaction summary
/model <name>        switch model mid-conversation
/save <name>         save session copy as <name>.json and adopt it
/new                 clear dialogue, keep persona, fresh session file
/quit                save and exit`)

	case "quit", "exit", "bye":
		return true

	case "stats":
		fmt.Printf("persona: %s   model: %s\nturns: %d   live tokens: ~%d / budget %d\nsummary tokens: ~%d\nfile: %s\n",
			sess.Persona, *model, len(sess.Messages)/2, promptEstimate(sess.buildPrompt()),
			*budget, estimateTokens(sess.Summary), *path)

	case "summary":
		if sess.Summary == "" {
			fmt.Println("(no summary yet)")
		} else {
			fmt.Println(sess.Summary)
		}

	case "model":
		if arg == "" {
			fmt.Println("usage: /model <name>")
		} else {
			*model = arg
			sess.Model = arg
			fmt.Printf("switched to %s (history carries over)\n", arg)
		}

	case "save":
		if arg == "" {
			fmt.Println("usage: /save <name>")
		} else {
			target := filepath.Join(dir, time.Now().Format("2006-01-02-150405")+"-"+arg+".json")
			if err := sess.Save(target); err != nil {
				fmt.Fprintf(os.Stderr, "[error] %v\n", err)
			} else {
				*path = target
				fmt.Printf("saved: %s\n", target)
			}
		}

	case "new":
		*sessPtr = &Session{Persona: sess.Persona, Model: *model, System: sess.System}
		*path = filepath.Join(dir, time.Now().Format("2006-01-02-150405")+".json")
		fmt.Printf("new session: %s\n", *path)

	default:
		fmt.Printf("unknown command: /%s (try /help)\n", cmd)
	}
	return false
}
