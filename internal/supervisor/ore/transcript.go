package ore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/usr-lex/swarmageddon/internal/supervisor/agent"
)

// Transcript implements agent.Transcriber from the thread's items
// (thread/read with turns).
func (d *Driver) Transcript(ctx context.Context, budget int) string {
	d.mu.Lock()
	c, thread := d.client, d.thread
	d.mu.Unlock()
	if c == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var res struct {
		Thread struct {
			Turns []struct {
				Items []json.RawMessage `json:"items"`
			} `json:"turns"`
		} `json:"thread"`
	}
	if err := c.Call(ctx, "thread/read", map[string]any{"threadId": thread, "includeTurns": true}, &res); err != nil {
		d.Log.Warn("reading the thread for a transcript", "err", err)
		return ""
	}
	var lines []string
	for _, t := range res.Thread.Turns {
		for _, raw := range t.Items {
			if l := itemLine(raw); l != "" {
				lines = append(lines, l)
			}
		}
	}
	return agent.Tail(lines, budget)
}

// itemLine renders one thread item.
func itemLine(raw json.RawMessage) string {
	var it struct {
		Type             string `json:"type"`
		Text             string `json:"text"`
		Command          string `json:"command"`
		AggregatedOutput string `json:"aggregatedOutput"`
		Content          []struct {
			Text string `json:"text"`
		} `json:"content"`
		Changes json.RawMessage `json:"changes"`
	}
	if json.Unmarshal(raw, &it) != nil {
		return ""
	}
	switch it.Type {
	case "userMessage":
		var s string
		for _, c := range it.Content {
			s += c.Text
		}
		return "user: " + agent.Clip(s, 2000)
	case "agentMessage":
		return "assistant: " + agent.Clip(it.Text, 2000)
	case "commandExecution":
		return fmt.Sprintf("command: %s => %s", agent.Clip(it.Command, 500), agent.Clip(it.AggregatedOutput, 300))
	case "fileChange":
		return "file change: " + agent.Clip(string(it.Changes), 500)
	case "reasoning", "":
		return ""
	}
	return it.Type
}

// Interrupt implements agent.Interrupter: turn/interrupt on the active turn,
// then the notice is added to the thread without starting a turn.
func (d *Driver) Interrupt(ctx context.Context, notice string) error {
	d.mu.Lock()
	c, thread, active := d.client, d.thread, d.activeTurn
	d.mu.Unlock()
	if c == nil {
		return errors.New("ore is not running")
	}
	var errs []error
	if active != "" {
		errs = append(errs, c.Call(ctx, "turn/interrupt", map[string]any{"threadId": thread, "turnId": active}, nil))
	}
	item := map[string]any{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": notice}}}
	errs = append(errs, c.Call(ctx, "thread/inject_items", map[string]any{"threadId": thread, "items": []any{item}}, nil))
	return errors.Join(errs...)
}
