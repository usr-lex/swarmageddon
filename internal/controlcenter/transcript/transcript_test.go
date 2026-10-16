package transcript

import (
	"strings"
	"testing"
)

func parseAll(cli, in string) []Item {
	p := &Parser{CLI: cli}
	var out []Item
	for _, l := range strings.Split(strings.TrimSpace(in), "\n") {
		out = append(out, p.Parse([]byte(l))...)
	}
	return out
}

func TestClaude(t *testing.T) {
	in := `{"type":"summary","summary":"x"}
{"type":"user","uuid":"u1","timestamp":"2026-10-16T10:00:00.000Z","message":{"role":"user","content":"fix the build"}}
{"type":"user","uuid":"u0","isMeta":true,"message":{"role":"user","content":"<command-name>"}}
{"type":"assistant","uuid":"a1","timestamp":"2026-10-16T10:00:01Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"Looking."},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go   build ./...","description":"build"}}]}}
{"type":"user","uuid":"u2","timestamp":"2026-10-16T10:00:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"ok"}],"is_error":true}]}}
{"type":"system","uuid":"s1","content":"Conversation compacted"}
not json`
	got := parseAll("claude", in)
	if len(got) != 5 {
		t.Fatalf("items %+v", got)
	}
	if got[0].Role != RoleUser || got[0].Text != "fix the build" || got[0].ID != "u1" || got[0].At.IsZero() {
		t.Errorf("user %+v", got[0])
	}
	if got[1].Role != RoleAssistant || got[1].Text != "Looking." || got[1].ID != "a1#1" {
		t.Errorf("assistant %+v", got[1])
	}
	if got[2].Role != RoleTool || got[2].ToolName != "Bash" || got[2].ToolInput != "go build ./..." {
		t.Errorf("tool %+v", got[2])
	}
	if got[3].Role != RoleToolResult || got[3].ToolName != "Bash" || got[3].ToolResult != "error: ok" {
		t.Errorf("result %+v", got[3])
	}
	if got[4].Role != RoleSystem || got[4].Text != "Conversation compacted" {
		t.Errorf("system %+v", got[4])
	}
}

func TestOre(t *testing.T) {
	in := `{"timestamp":"2026-10-16T10:00:00Z","type":"session_meta","payload":{"id":"th1","cwd":"/home/hermes/work"}}
{"timestamp":"2026-10-16T10:00:00Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"instructions"}]}}
{"timestamp":"2026-10-16T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}
{"timestamp":"2026-10-16T10:00:01Z","type":"event_msg","payload":{"type":"user_message","message":"hello"}}
{"timestamp":"2026-10-16T10:00:02Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"ls -la\"}","call_id":"c1"}}
{"timestamp":"2026-10-16T10:00:03Z","type":"response_item","payload":{"type":"function_call_output","call_id":"c1","output":"total 0"}}
{"timestamp":"2026-10-16T10:00:04Z","type":"response_item","payload":{"type":"custom_tool_call","name":"apply_patch","input":"*** Begin Patch","call_id":"c2"}}
{"timestamp":"2026-10-16T10:00:05Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"c2","output":[{"type":"input_text","text":"Success"}]}}
{"timestamp":"2026-10-16T10:00:06Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Done."}]}}`
	got := parseAll("ore", in)
	want := []struct{ role, name, text string }{
		{RoleSystem, "", "session th1 started in /home/hermes/work"},
		{RoleUser, "", "hello"},
		{RoleTool, "exec_command", "ls -la"},
		{RoleToolResult, "exec_command", "total 0"},
		{RoleTool, "apply_patch", "*** Begin Patch"},
		{RoleToolResult, "apply_patch", "Success"},
		{RoleAssistant, "", "Done."},
	}
	if len(got) != len(want) {
		t.Fatalf("items %+v", got)
	}
	for i, w := range want {
		g := got[i]
		text := g.Text + g.ToolInput + g.ToolResult
		if g.Role != w.role || g.ToolName != w.name || text != w.text {
			t.Errorf("item %d = %+v, want %+v", i, g, w)
		}
	}
}

func TestClipAndCommand(t *testing.T) {
	if c := Clip(strings.Repeat("é", 10), 8); c != "éé..." {
		t.Errorf("clip %q", c)
	}
	if d := ProjectDir("/home/athena/work/my.repo"); d != "-home-athena-work-my-repo" {
		t.Errorf("project dir %q", d)
	}
	cmd := Command("athena", "claude", "/home/athena/work/", 200)
	if cmd[0] != "runuser" || cmd[2] != "athena" || cmd[len(cmd)-3] != "-home-athena-work" || cmd[len(cmd)-2] != "200" || cmd[len(cmd)-1] != "claude" {
		t.Errorf("command %q", cmd)
	}
	if !strings.Contains(Script, "tail -n \"$2\" -F") {
		t.Error("script does not follow the file")
	}
}
