package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// ---------------------------------------------------------------------------
// Session types and persistence
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
// which sort lexicographically in chronological order. Saved copies
// (/save) also carry a timestamp prefix, preserving the ordering.
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