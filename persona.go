package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Persona loading
// ---------------------------------------------------------------------------

// Persona is a named system prompt plus inference defaults, loaded from a
// small file with "key: value" front matter above the system prompt.
type Persona struct {
	Name        string
	Model       string
	System      string
	Temperature float64 // 0 = unset, use server default (limits exact-0 tuning)
}

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
// Persona subcommands
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

// sessionsCommand lists a persona's session files, newest first, with turn
// counts and a one-line preview of each conversation's first user message.
// With no argument, iterates every persona.
func sessionsCommand(personasDir, dataRoot, name string) {
	if name != "" {
		p, err := loadPersona(filepath.Join(personasDir, name+".md"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "persona %q: %v\n", name, err)
			os.Exit(1)
		}
		sessionsCommandOne(p, dataRoot)
		return
	}

	entries, err := os.ReadDir(personasDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "(no personas yet — create one with: persona init <name>)\n")
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
		sessionsCommandOne(p, dataRoot)
	}
}

// sessionsCommandOne prints the session listing for one loaded persona.
func sessionsCommandOne(p *Persona, dataRoot string) {
	sdir := filepath.Join(dataRoot, p.Name, "sessions")
	entries, err := os.ReadDir(sdir)
	if err != nil {
		fmt.Printf("%s — no sessions\n", p.Name)
		return
	}

	// Collect then reverse: os.ReadDir returns name-ascending order,
	// newest-first display wants descending.
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		fmt.Printf("%s — no sessions\n", p.Name)
		return
	}

	fmt.Printf("%s — %d session%s\n", p.Name, len(names), plural(len(names)))
	for i := len(names) - 1; i >= 0; i-- {
		path := filepath.Join(sdir, names[i])
		s, err := LoadSession(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s: unreadable (%v)\n", names[i], err)
			continue
		}
		fmt.Printf("  %-32s %3d turns   %s\n", names[i], len(s.Messages)/2, preview(s))
	}
}

// preview extracts a flattened, truncated first-user-message summary.
func preview(s *Session) string {
	for _, m := range s.Messages {
		if m.Role != "user" {
			continue
		}
		text := strings.Join(strings.Fields(m.Content), " ") // collapse whitespace/newlines
		const width = 50
		if len(text) > width {
			trunc := []rune(text[:width])
			return string(trunc) + "…"
		}
		return "\"" + text + "\""
	}
	return "(no user message)"
}

// plural returns "" or "s" for count.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
