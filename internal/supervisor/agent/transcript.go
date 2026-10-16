package agent

import (
	"context"
	"strings"
)

// Transcriber is implemented by drivers that can return the tail of the
// session transcript (for the advisor and the warden).
type Transcriber interface {
	Transcript(ctx context.Context, budget int) string
}

// Interrupter is implemented by drivers that can stop the current turn
// (the warden's interruptOnHigh). notice is shown to the agent.
type Interrupter interface {
	Interrupt(ctx context.Context, notice string) error
}

// Tail keeps the last entries of lines that fit in budget bytes, oldest first.
func Tail(lines []string, budget int) string {
	var kept []string
	size := 0
	for i := len(lines) - 1; i >= 0; i-- {
		if size+len(lines[i])+1 > budget {
			break
		}
		kept = append(kept, lines[i])
		size += len(lines[i]) + 1
	}
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	return strings.Join(kept, "\n")
}

// Clip shortens s to n bytes on one line.
func Clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = strings.ToValidUTF8(s[:n-3], "") + "..."
	}
	return s
}
