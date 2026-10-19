# persona

A minimal, stdlib-only Go wrapper for chatting with local Ollama models through persistent, named personas. Each persona is a plain text file defining a system prompt and inference defaults; conversations are saved to per-persona session files that can be resumed at any time.

Built for CPU-only local inference workflows: streaming replies with a live thinking indicator, context budgeting, optional summarization compaction for long-running conversations, and session browsing for finding the right conversation to resume.

## Features

- Multiple personas defined as simple front-matter text files
- Automatic session persistence (saved after every exchange, crash-safe)
- Resume the latest session or a specific session file by name
- Session listing with turn counts and first-message previews
- Streaming token output with an elapsed-time thinking spinner
- In-REPL message history viewing with eviction disclosure
- Token-budget trimming with optional summarization of evicted history
- Mid-conversation model switching with history carried over
- No external dependencies; Go standard library only

## Requirements

- Go 1.21 or newer
- Ollama running locally on the default port (http://localhost:11434)

## Build

    go mod init persona
    go build -o persona .

Optionally install somewhere on PATH:

    sudo cp persona /usr/local/bin/

## Directory Layout

    ~/.config/persona/personas/          persona definition files (*.md)
    ~/.local/share/persona/<name>/sessions/   per-persona session history (*.json)

Directories are created automatically as needed.

## Source Layout

The program is organized by concern, all within package main:

    main.go       entry point: argument dispatch, flags, session resolution, REPL loop
    persona.go    persona type and parsing; list, init, and sessions subcommands
    session.go    session types, save/load, session file discovery
    prompt.go     token estimation, prompt assembly, eviction policy
    ollama.go     HTTP client for Ollama's /api/chat: streaming, completion, compaction
    ui.go         thinking-spinner activity indicator

## Persona Files

A persona is a text file with optional key: value front matter, followed by the system prompt as free text. Create one with:

    persona init marcus

Output:

    created: /home/lee8oi/.config/persona/personas/marcus.md
    next: edit the file (system prompt + front matter), then run 'persona marcus'

The resulting file looks like this (note: four-space indented code blocks inside this document; plain text below the closing --- is the system prompt):

    ---
    name: marcus
    model: qwen3:8b
    temperature: 0.8
    ---

    You are Marcus Aurelius, Roman emperor and Stoic philosopher...

Front matter keys:

- name: persona identifier; also names its session directory. Falls back to the filename if omitted.
- model: required. Ollama model tag to run.
- temperature: optional. Omitted from requests when unset, deferring to the server default. Note that exactly 0 cannot be distinguished from unset; use 0.01 for near-deterministic output.

If a file declares a name that differs from its filename, session storage follows the front-matter name.

## Usage

    persona                          show help and list personas
    persona list                     list personas with session counts
    persona init <name>              create a starter persona file
    persona sessions [<name>]        list sessions for a persona (or all)
    persona <name>                   resume the latest session
    persona <name> -new              start a fresh session
    persona <name> -resume <file>    resume a specific session file
    persona <name> -model <tag>      override the persona's model

Examples:

    persona marcus                    continue the most recent Marcus session
    persona marcus -new               start over with a blank session
    persona marcus -resume 2026-10-09-154001.json
    persona marcus -model qwen3:8b    try a smaller model with same history
    persona sessions marcus           browse Marcus sessions to pick one

The -resume flag accepts an absolute path, a cwd-relative path, or a bare filename. Bare filenames resolve against the persona's sessions directory, so a session name copied from persona sessions works from any directory.

### Flags

    -endpoint <url>     Ollama chat endpoint (default http://localhost:11434/api/chat)
    -new                start a new session instead of resuming the latest
    -resume <file>      resume this session file instead of the latest
    -model <tag>        override the persona's model for this run
    -budget <n>         approximate token budget for live history (default 16000)
    -ctx <n>            override num_ctx (0 = model default)
    -compact            summarize evicted turns instead of dropping them

## REPL Commands

Once a session is running, slash commands are available:

    /stats              session statistics (turns, token estimate, file path)
    /history [N]        show live messages (last N; default all)
    /summary            show the current compaction summary
    /model <name>       switch model mid-conversation
    /save <name>        save a session copy and adopt it
    /new                clear dialogue, keep persona, fresh session file
    /quit               save and exit

Plain input lines are sent to the model; empty lines are ignored.

/history prints the live dialogue with role headers. It reports what the model currently sees, not the full lifetime of the conversation: turns hidden by a smaller N, and any turns evicted by trimming, are disclosed in footers. Evicted turns are only recoverable through the compaction summary (see below); /history says so explicitly when they are not.

## Thinking Indicator

While waiting for the first token, the REPL shows an animated spinner with the model tag and an elapsed-seconds counter:

    [qwen3:8b] thinking - 23s

CPU-only prefill of a large prompt can take tens of seconds, and reasoning models emit silence while thinking; the elapsed counter distinguishes a working model from a hung one. The indicator disappears when the first visible token arrives. It is disabled automatically when output is piped or redirected, so logged sessions stay clean.

## Sessions and Compaction

Sessions are saved as JSON after every exchange, so an unexpected exit loses nothing. Each session stores the persona name, model, system prompt, rolling summary, and full dialogue history.

Session filenames are timestamps (2006-01-02-150405.json), which sort chronologically; a bare resume picks the newest. Session copies made with /save carry a timestamp prefix plus the given name (for example 2026-10-19-153000-notes.json) so that every file in the sessions directory keeps the same chronological sort order. Sessions listed with persona sessions appear newest first, with turn counts and a preview of the first user message.

Live history is capped at the token budget (default ~16000 tokens). Trimming happens only after a successful reply, so a failed exchange never discards history. When the cap is exceeded, the oldest user/assistant pairs are evicted. With -compact, evicted pairs are merged into a running summary by the same model, preserving facts, decisions, and unresolved questions at a fraction of the token cost. Without it, evicted turns are simply dropped. Either way, the session records an all-time eviction count so /history can disclose how much is no longer live.

Token counts are estimated at ~4 characters per token, which is deliberately conservative for English prose.

## License

Personal project; no license terms defined yet.