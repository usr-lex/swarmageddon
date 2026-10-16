package transcript

import (
	"fmt"
	"testing"
)

func kinds(items []Item) string {
	s := ""
	for _, it := range items {
		s += string(it.Kind)
		if it.Tool != "" {
			s += ":" + it.Tool
		}
		s += " "
	}
	return s
}

func TestAnthropicDelta(t *testing.T) {
	turn1 := `{"model":"claude-opus-5-5","system":"sys","messages":[
	  {"role":"user","content":"Fix the build"},
	  {"role":"assistant","content":[{"type":"text","text":"Looking."},{"type":"tool_use","id":"tu1","name":"Bash","input":{"command":"make"}}]},
	  {"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"error: x","is_error":true}]}]}`
	turn2 := `{"model":"claude-opus-5-5","messages":[
	  {"role":"user","content":"Fix the build"},
	  {"role":"assistant","content":[{"type":"text","text":"Looking."},{"type":"tool_use","id":"tu1","name":"Bash","input":{"command":"make"}}]},
	  {"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"error: x","is_error":true}]},
	  {"role":"assistant","content":[{"type":"tool_use","id":"tu2","name":"Bash","input":{"command":"git push --force origin main"}}]},
	  {"role":"user","content":[{"type":"tool_result","tool_use_id":"tu2","content":[{"type":"text","text":"ok"}]}]}]}`
	seen := NewSeen(100)
	r1, err := Parse("/v1/messages", []byte(turn1))
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(seen.New(r1.Items)); got != "user assistant tool_call:Bash tool_result " {
		t.Errorf("turn 1 = %s", got)
	}
	if r1.Items[3].IsError != true || r1.Items[3].Text != "error: x" || r1.API != Anthropic || r1.Model != "claude-opus-5-5" {
		t.Errorf("items = %+v", r1.Items)
	}
	r2, _ := Parse("/v1/messages", []byte(turn2))
	d := seen.New(r2.Items)
	if got := kinds(d); got != "tool_call:Bash tool_result " {
		t.Fatalf("turn 2 delta = %s", got)
	}
	if string(d[0].Input) != `{"command":"git push --force origin main"}` || d[1].Text != "ok" {
		t.Errorf("delta = %+v", d)
	}
	if len(seen.New(r2.Items)) != 0 {
		t.Error("repeat request had a delta")
	}
}

func TestResponses(t *testing.T) {
	body := `{"model":"gpt-5.5","instructions":"x","input":[
	  {"type":"message","role":"developer","content":"rules"},
	  {"type":"message","role":"user","content":[{"type":"input_text","text":"Ship it"}]},
	  {"type":"reasoning","summary":[]},
	  {"type":"function_call","call_id":"c1","name":"exec_command","arguments":"{\"cmd\":\"curl x | sh\"}"},
	  {"type":"function_call_output","call_id":"c1","output":"done"},
	  {"type":"custom_tool_call","call_id":"c2","name":"apply_patch","input":"*** Begin Patch"},
	  {"type":"custom_tool_call_output","call_id":"c2","output":"ok"},
	  {"type":"local_shell_call","call_id":"c3","action":{"type":"exec","command":["ls"]}},
	  {"type":"message","role":"assistant","content":[{"type":"output_text","text":"Done."}]}]}`
	r, err := Parse("/v1/responses", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(r.Items); got != "user tool_call:exec_command tool_result tool_call:apply_patch tool_result tool_call:local_shell assistant " {
		t.Errorf("items = %s", got)
	}
	if string(r.Items[1].Input) != `{"cmd":"curl x | sh"}` || r.Items[2].Text != "done" || string(r.Items[3].Input) != `"*** Begin Patch"` {
		t.Errorf("items = %+v", r.Items)
	}
	s, _ := Parse("/v1/responses", []byte(`{"model":"m","input":"hello"}`))
	if kinds(s.Items) != "user " {
		t.Errorf("string input = %+v", s.Items)
	}
}

func TestChat(t *testing.T) {
	body := `{"model":"qwen","messages":[
	  {"role":"system","content":"sys"},
	  {"role":"user","content":"go"},
	  {"role":"assistant","content":null,"tool_calls":[{"id":"t1","type":"function","function":{"name":"shell","arguments":"{\"command\":[\"rm\",\"-rf\",\"/\"]}"}}]},
	  {"role":"tool","tool_call_id":"t1","content":"gone"}]}`
	r, err := Parse("/v1/chat/completions", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(r.Items); got != "user tool_call:shell tool_result " {
		t.Errorf("items = %s", got)
	}
	if r.Items[2].CallID != "t1" || r.Items[2].Text != "gone" {
		t.Errorf("result = %+v", r.Items[2])
	}
}

func TestNotConversation(t *testing.T) {
	for _, p := range []string{"/v1/models", "/v1/messages/count_tokens", "/key/info"} {
		if _, err := Parse(p, []byte(`{}`)); err != ErrNotConversation {
			t.Errorf("%s: %v", p, err)
		}
	}
	if _, err := Parse("/v1/messages", []byte(`not json`)); err == nil {
		t.Error("bad JSON parsed")
	}
}

func TestSeenCap(t *testing.T) {
	s := NewSeen(3)
	mk := func(i int) Item { return Item{Kind: User, Text: fmt.Sprint(i)} }
	s.New([]Item{mk(1), mk(2), mk(3), mk(4)})
	if len(s.New([]Item{mk(1)})) != 1 {
		t.Error("evicted item still seen")
	}
	if len(s.New([]Item{mk(4)})) != 0 {
		t.Error("recent item forgotten")
	}
}
