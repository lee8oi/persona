package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// REPL commands and usage
// ---------------------------------------------------------------------------

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
			// Timestamp prefix keeps lexicographic sort chronological:
			// plain <name>.json would sort after all timestamped files
			// and hijack bare "resume latest" forever.
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
