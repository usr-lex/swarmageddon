package mockmodel

import (
	"encoding/json"
	"fmt"
)

// --- Anthropic Messages ---

func parseAnthropic(body []byte) (*conversation, error) {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	c := &conversation{}
	for _, t := range req.Tools {
		c.tools = append(c.tools, toolDef{name: t.Name})
	}
	for _, m := range req.Messages {
		cm := convMsg{role: m.Role}
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			cm.texts = []string{s}
		} else {
			var blocks []struct {
				Type    string          `json:"type"`
				Text    string          `json:"text"`
				Content json.RawMessage `json:"content"`
			}
			_ = json.Unmarshal(m.Content, &blocks)
			for _, b := range blocks {
				switch b.Type {
				case "text":
					cm.texts = append(cm.texts, b.Text)
				case "tool_result":
					cm.toolOutputs++
				}
			}
		}
		c.msgs = append(c.msgs, cm)
	}
	return c, nil
}

func anthropicContent(r reply) ([]any, string) {
	var content []any
	if r.text != "" {
		content = append(content, map[string]any{"type": "text", "text": r.text})
	}
	stop := "end_turn"
	if r.call != nil {
		content = append(content, map[string]any{"type": "tool_use", "id": fmt.Sprintf("toolu_mock_%d", r.n), "name": r.call.name, "input": r.call.input})
		stop = "tool_use"
	}
	return content, stop
}

func anthropicMessage(r reply, model string) map[string]any {
	content, stop := anthropicContent(r)
	return map[string]any{
		"id": fmt.Sprintf("msg_mock_%d", r.n), "type": "message", "role": "assistant", "model": model,
		"content": content, "stop_reason": stop, "stop_sequence": nil, "usage": usage,
	}
}

func anthropicEvents(r reply, model string) []event {
	evs := []event{{"message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": fmt.Sprintf("msg_mock_%d", r.n), "type": "message", "role": "assistant", "model": model,
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
		"usage": map[string]int{"input_tokens": 10, "output_tokens": 1},
	}}}}
	idx := 0
	if r.text != "" {
		evs = append(evs,
			event{"content_block_start", map[string]any{"type": "content_block_start", "index": idx, "content_block": map[string]any{"type": "text", "text": ""}}},
			event{"content_block_delta", map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]any{"type": "text_delta", "text": r.text}}},
			event{"content_block_stop", map[string]any{"type": "content_block_stop", "index": idx}})
		idx++
	}
	stop := "end_turn"
	if r.call != nil {
		evs = append(evs,
			event{"content_block_start", map[string]any{"type": "content_block_start", "index": idx, "content_block": map[string]any{
				"type": "tool_use", "id": fmt.Sprintf("toolu_mock_%d", r.n), "name": r.call.name, "input": map[string]any{}}}},
			event{"content_block_delta", map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]any{"type": "input_json_delta", "partial_json": r.args()}}},
			event{"content_block_stop", map[string]any{"type": "content_block_stop", "index": idx}})
		stop = "tool_use"
	}
	return append(evs,
		event{"message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 5}}},
		event{"message_stop", map[string]any{"type": "message_stop"}})
}

// --- OpenAI Responses ---

func parseResponses(body []byte) (*conversation, error) {
	var req struct {
		Input json.RawMessage `json:"input"`
		Tools []struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	c := &conversation{}
	for _, t := range req.Tools {
		if t.Name != "" {
			c.tools = append(c.tools, toolDef{name: t.Name, custom: t.Type == "custom"})
		} else if t.Type != "" {
			c.tools = append(c.tools, toolDef{name: t.Type})
		}
	}
	var s string
	if json.Unmarshal(req.Input, &s) == nil {
		c.msgs = []convMsg{{role: "user", texts: []string{s}}}
		return c, nil
	}
	var items []struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	_ = json.Unmarshal(req.Input, &items)
	for _, it := range items {
		switch it.Type {
		case "message", "":
			cm := convMsg{role: it.Role}
			if json.Unmarshal(it.Content, &s) == nil {
				cm.texts = []string{s}
			} else {
				var parts []struct {
					Text string `json:"text"`
				}
				_ = json.Unmarshal(it.Content, &parts)
				for _, p := range parts {
					cm.texts = append(cm.texts, p.Text)
				}
			}
			c.msgs = append(c.msgs, cm)
		case "function_call_output", "custom_tool_call_output", "local_shell_call_output":
			c.msgs = append(c.msgs, convMsg{role: "tool", toolOutputs: 1})
		}
	}
	return c, nil
}

func responsesOutput(r reply) []any {
	var out []any
	if r.text != "" {
		out = append(out, map[string]any{
			"type": "message", "id": fmt.Sprintf("msg_mock_%d", r.n), "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": r.text, "annotations": []any{}}},
		})
	}
	if r.call != nil {
		id, callID := fmt.Sprintf("fc_mock_%d", r.n), fmt.Sprintf("call_mock_%d", r.n)
		if r.call.isCust {
			out = append(out, map[string]any{"type": "custom_tool_call", "id": id, "call_id": callID, "name": r.call.name, "input": r.call.custom, "status": "completed"})
		} else {
			fc := map[string]any{"type": "function_call", "id": id, "call_id": callID, "name": r.call.name, "arguments": r.args(), "status": "completed"}
			if r.call.namespace != "" {
				fc["namespace"] = r.call.namespace
			}
			out = append(out, fc)
		}
	}
	return out
}

// mockCreatedAt is a fixed created_at: LiteLLM requires the field, and the
// golden files need it stable.
const mockCreatedAt = 1700000000

var responsesUsage = map[string]any{
	"input_tokens": 10, "input_tokens_details": map[string]int{"cached_tokens": 0},
	"output_tokens": 5, "output_tokens_details": map[string]int{"reasoning_tokens": 0}, "total_tokens": 15,
}

func responsesObject(r reply, model string) map[string]any {
	return map[string]any{
		"id": fmt.Sprintf("resp_mock_%d", r.n), "object": "response", "created_at": mockCreatedAt, "status": "completed", "model": model,
		"output": responsesOutput(r), "usage": responsesUsage,
	}
}

func responsesEvents(r reply, model string) []event {
	id := fmt.Sprintf("resp_mock_%d", r.n)
	evs := []event{{"response.created", map[string]any{"type": "response.created", "response": map[string]any{
		"id": id, "object": "response", "created_at": mockCreatedAt, "status": "in_progress", "model": model, "output": []any{}}}}}
	for i, item := range responsesOutput(r) {
		evs = append(evs, event{"response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": i, "item": item}})
	}
	return append(evs, event{"response.completed", map[string]any{"type": "response.completed", "response": responsesObject(r, model)}})
}

// --- OpenAI Chat Completions ---

func parseChat(body []byte) (*conversation, error) {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	c := &conversation{}
	for _, t := range req.Tools {
		c.tools = append(c.tools, toolDef{name: t.Function.Name})
	}
	for _, m := range req.Messages {
		if m.Role == "tool" {
			c.msgs = append(c.msgs, convMsg{role: "tool", toolOutputs: 1})
			continue
		}
		cm := convMsg{role: m.Role}
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			cm.texts = []string{s}
		} else {
			var parts []struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(m.Content, &parts)
			for _, p := range parts {
				cm.texts = append(cm.texts, p.Text)
			}
		}
		c.msgs = append(c.msgs, cm)
	}
	return c, nil
}

func chatToolCalls(r reply) []any {
	return []any{map[string]any{"index": 0, "id": fmt.Sprintf("call_mock_%d", r.n), "type": "function",
		"function": map[string]any{"name": r.call.name, "arguments": r.args()}}}
}

func chatObject(r reply, model string) map[string]any {
	msg := map[string]any{"role": "assistant", "content": r.text}
	finish := "stop"
	if r.call != nil {
		msg["tool_calls"] = chatToolCalls(r)
		finish = "tool_calls"
	}
	return map[string]any{
		"id": fmt.Sprintf("chatcmpl-mock-%d", r.n), "object": "chat.completion", "created": 0, "model": model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
		"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	}
}

func chatEvents(r reply, model string) []event {
	chunk := func(delta map[string]any, finish any) event {
		return event{"", map[string]any{
			"id": fmt.Sprintf("chatcmpl-mock-%d", r.n), "object": "chat.completion.chunk", "created": 0, "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		}}
	}
	evs := []event{chunk(map[string]any{"role": "assistant", "content": r.text}, nil)}
	finish := "stop"
	if r.call != nil {
		evs = append(evs, chunk(map[string]any{"tool_calls": chatToolCalls(r)}, nil))
		finish = "tool_calls"
	}
	evs = append(evs, chunk(map[string]any{}, finish))
	evs = append(evs, event{"", map[string]any{
		"id": fmt.Sprintf("chatcmpl-mock-%d", r.n), "object": "chat.completion.chunk", "created": 0, "model": model,
		"choices": []any{}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	}})
	return append(evs, event{"", "[DONE]"})
}
