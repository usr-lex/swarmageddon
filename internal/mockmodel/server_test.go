package mockmodel

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

const testScript = `
default: fine
turns:
  - match: "MARK-CMD"
    steps:
      - command: "echo hi"
      - text: "ran it"
  - match: "MARK-WRITE"
    steps:
      - write: {path: notes.txt, content: "a\nb"}
      - text: "wrote it"
  - match: "MARK-MCP"
    steps:
      - mcp: {server: swarm, tool: send_message, input: {to: hermes, body: hello}}
      - text: "sent"
`

func post(t *testing.T, srv *httptest.Server, path, body string) string {
	t.Helper()
	resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("%s: %d %s", path, resp.StatusCode, b)
	}
	return string(b)
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	file := filepath.Join("testdata", name+".golden")
	if *update {
		os.MkdirAll("testdata", 0o755)
		os.WriteFile(file, []byte(got), 0o644)
		return
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("%s mismatch:\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

func newServer(t *testing.T) (*Server, *httptest.Server) {
	sc, err := ParseScript([]byte(testScript))
	if err != nil {
		t.Fatal(err)
	}
	s := New(sc)
	hs := httptest.NewServer(s)
	t.Cleanup(hs.Close)
	return s, hs
}

func TestAnthropicGolden(t *testing.T) {
	_, hs := newServer(t)
	tools := `"tools":[{"name":"Bash"},{"name":"Write"},{"name":"mcp__swarm__send_message"}]`
	golden(t, "anthropic_cmd_stream", post(t, hs, "/v1/messages?beta=true",
		`{"model":"m","stream":true,`+tools+`,"messages":[{"role":"user","content":[{"type":"text","text":"please MARK-CMD"}]}]}`))
	golden(t, "anthropic_cmd_done_stream", post(t, hs, "/v1/messages",
		`{"model":"m","stream":true,`+tools+`,"messages":[{"role":"user","content":"please MARK-CMD"},{"role":"assistant","content":[{"type":"tool_use"}]},{"role":"user","content":[{"type":"tool_result","content":"hi"}]}]}`))
	golden(t, "anthropic_mcp", post(t, hs, "/v1/messages",
		`{"model":"m",`+tools+`,"messages":[{"role":"user","content":"MARK-MCP"}]}`))
	golden(t, "anthropic_default", post(t, hs, "/v1/messages",
		`{"model":"m","messages":[{"role":"user","content":"MARK-CMD but no tools"}]}`))
}

func TestResponsesGolden(t *testing.T) {
	_, hs := newServer(t)
	tools := `"tools":[{"type":"function","name":"exec_command"},{"type":"custom","name":"apply_patch"}]`
	golden(t, "responses_cmd_stream", post(t, hs, "/v1/responses",
		`{"model":"m","stream":true,`+tools+`,"input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"sys"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"MARK-CMD"}]}]}`))
	golden(t, "responses_write_stream", post(t, hs, "/v1/responses",
		`{"model":"m","stream":true,`+tools+`,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"MARK-WRITE"}]}]}`))
	golden(t, "responses_mcp_namespace", post(t, hs, "/v1/responses",
		`{"model":"m","tools":[{"type":"function","name":"exec_command"},{"type":"namespace","name":"mcp__swarm","tools":[{"type":"function","name":"send_message"}]}],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"MARK-MCP"}]}]}`))
	golden(t, "responses_steer_stream", post(t, hs, "/v1/responses",
		`{"model":"m","stream":true,`+tools+`,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"MARK-CMD"}]},{"type":"function_call"},{"type":"function_call_output"},{"type":"message","role":"user","content":[{"type":"input_text","text":"steered in"}]}]}`))
}

func TestChatGolden(t *testing.T) {
	_, hs := newServer(t)
	tools := `"tools":[{"type":"function","function":{"name":"exec_command"}}]`
	golden(t, "chat_cmd_stream", post(t, hs, "/v1/chat/completions",
		`{"model":"m","stream":true,`+tools+`,"messages":[{"role":"user","content":"MARK-CMD"}]}`))
	golden(t, "chat_done", post(t, hs, "/v1/chat/completions",
		`{"model":"m",`+tools+`,"messages":[{"role":"user","content":"MARK-CMD"},{"role":"assistant","content":""},{"role":"tool","content":"hi"}]}`))
}

func TestRequestLog(t *testing.T) {
	s, hs := newServer(t)
	post(t, hs, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"x"}]}`)
	resp, err := http.Get(hs.URL + "/_mock/requests")
	if err != nil {
		t.Fatal(err)
	}
	var log []Request
	json.NewDecoder(resp.Body).Decode(&log)
	if len(log) != 1 || log[0].Reply != "fine" || len(s.Requests()) != 1 {
		t.Errorf("log %+v", log)
	}
}

func TestParseScriptErrors(t *testing.T) {
	if _, err := ParseScript([]byte("turns: [{steps: []}]")); err == nil {
		t.Error("empty match accepted")
	}
	if _, err := ParseScript([]byte("turns: [{match: x, bogus: 1}]")); err == nil {
		t.Error("unknown field accepted")
	}
	if !bytes.Contains(encodeSSE([]event{{"", "[DONE]"}}), []byte("data: [DONE]\n\n")) {
		t.Error("done")
	}
}
