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
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

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

		fmt.Printf("\n%s> ", p.Name)
		reply, err := streamChat(a, model, sess.buildPrompt())
		fmt.Println()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[error] %v (turn discarded, retry)\n", err)
			sess.Messages = sess.Messages[:len(sess.Messages)-1]
			continue
		}
		sess.Messages = append(sess.Messages, Message{Role: "assistant", Content: reply})

		// Trim (and optionally compact) only after a successful reply, so
		// a failed exchange never evicts history or summarizes into a
		// dead server. The outgoing request may exceed budget by one
		// exchange; corrected on the following turn.
		evicted := sess.trim(*budget)
		if len(evicted) > 0 {
			if *compact {
				sess.Summary = summarize(a, model, evicted, sess.Summary)
			}
			fmt.Printf("[context trimmed: %d turns evicted]\n", len(evicted))
		}

		sess.Save(sessionPath) // crash-safe: persist after every exchange
	}

	if err := sess.Save(sessionPath); err != nil {
		fmt.Fprintf(os.Stderr, "[error] final save failed: %v\n", err)
	}
	fmt.Printf("\nsession saved: %s\n", sessionPath)
}