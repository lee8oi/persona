package main

import "unicode/utf8"

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