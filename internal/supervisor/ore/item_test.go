package ore

import "testing"

func TestItemCall(t *testing.T) {
	for in, want := range map[string]struct {
		tool, cmd string
		failed    bool
	}{
		`{"type":"commandExecution","command":"go test","status":"completed","exitCode":1}`:              {"exec", "go test", true},
		`{"type":"fileChange","changes":[{"path":"a.go"},{"path":"b.go"}],"status":"completed"}`:         {"patch", "a.go b.go", false},
		`{"type":"mcpToolCall","server":"swarm","tool":"send_message","status":"failed","arguments":{}}`: {"mcp__swarm__send_message", "{}", true},
	} {
		c, failed, ok := ItemCall([]byte(in))
		if !ok || c.Tool != want.tool || c.Command != want.cmd || failed != want.failed {
			t.Errorf("%s: %+v %v %v", in, c, failed, ok)
		}
	}
	if _, _, ok := ItemCall([]byte(`{"type":"agentMessage"}`)); ok {
		t.Error("agentMessage")
	}
}
