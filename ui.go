package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Activity indicator
// ---------------------------------------------------------------------------

// isTTY reports whether f is attached to a character device (a terminal),
// so interactive niceties can be skipped when output is piped or redirected.
func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// spinner animates a "thinking" indicator in place on the current line,
// erased once the first output token arrives. A nil *spinner (returned
// when stdout is not a TTY) accepts Stop calls as a no-op.
type spinner struct {
	stopOnce sync.Once
	stopped  chan struct{}
}

// startSpinner begins rendering "<label> | 12s" refreshed in place.
func startSpinner(label string) *spinner {
	if !isTTY(os.Stdout) {
		return nil
	}
	sp := &spinner{stopped: make(chan struct{})}
	go func() {
		frames := []string{"|", "/", "-", "\\"}
		start := time.Now()
		tick := time.NewTicker(120 * time.Millisecond)
		defer tick.Stop()
		i := 0
		for {
			select {
			case <-sp.stopped:
				return
			case <-tick.C:
				fmt.Fprintf(os.Stdout, "\r%s %s %2ds ", label,
					frames[i%len(frames)], int(time.Since(start).Seconds()))
				i++
			}
		}
	}()
	return sp
}

// Stop erases the spinner line. Idempotent; safe on a nil receiver.
func (sp *spinner) Stop() {
	if sp == nil {
		return
	}
	sp.stopOnce.Do(func() {
		close(sp.stopped)
		fmt.Print("\r" + strings.Repeat(" ", 48) + "\r")
	})
}
