package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/usr-lex/swarmageddon/internal/supervisor/agent"
)

// Transcript implements agent.Transcriber from the session's JSONL
// transcript (path reported by SessionStart). The entry format is internal
// to Claude Code; unknown entries are skipped.
func (d *Driver) Transcript(_ context.Context, budget int) string {
	d.mu.Lock()
	path := d.transcript
	d.mu.Unlock()
	if path == "" {
		return ""
	}
	return TranscriptTail(path, budget)
}

// Interrupt implements agent.Interrupter: a frame with priority "now"
// interrupts the running turn and shows notice to the agent.
func (d *Driver) Interrupt(_ context.Context, notice string) error {
	d.mu.Lock()
	sock, tok := d.socket, d.token
	d.mu.Unlock()
	if sock == "" {
		return fmt.Errorf("claude is not running")
	}
	return writeFrame(sock, tok, "warden", notice, "now")
}

// TranscriptTail renders the last entries of a Claude Code transcript.
func TranscriptTail(path string, budget int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var e struct {
			Type    string `json:"type"`
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil || (e.Type != "user" && e.Type != "assistant") {
			continue
		}
		var s string
		if json.Unmarshal(e.Message.Content, &s) == nil {
			lines = append(lines, e.Type+": "+agent.Clip(s, 2000))
			continue
		}
		var blocks []struct {
			Type    string          `json:"type"`
			Text    string          `json:"text"`
			Name    string          `json:"name"`
			Input   json.RawMessage `json:"input"`
			Content json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(e.Message.Content, &blocks)
		for _, b := range blocks {
			switch b.Type {
			case "text":
				lines = append(lines, e.Type+": "+agent.Clip(b.Text, 2000))
			case "tool_use":
				lines = append(lines, fmt.Sprintf("tool %s: %s", b.Name, agent.Clip(string(b.Input), 500)))
			case "tool_result":
				var rs string
				if json.Unmarshal(b.Content, &rs) != nil {
					rs = string(b.Content)
				}
				lines = append(lines, "result: "+agent.Clip(rs, 300))
			}
		}
	}
	return agent.Tail(lines, budget)
}
