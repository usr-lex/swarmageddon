package mockmodel

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Request is one logged model request.
type Request struct {
	N     int    `json:"n"`
	API   string `json:"api"` // anthropic, responses, chat
	Path  string `json:"path"`
	Model string `json:"model"`
	// Turn is the matched marker, Step the step index; Reply what was sent.
	Turn  string `json:"turn,omitempty"`
	Step  int    `json:"step"`
	Reply string `json:"reply"`
	Body  string `json:"body"`
}

// Server serves the mock model APIs.
type Server struct {
	mu     sync.Mutex
	script *Script
	n      int
	log    []Request
	Logger *slog.Logger
}

// New returns a server for script (nil means only default replies).
func New(s *Script) *Server {
	if s == nil {
		s = &Script{}
	}
	return &Server{script: s, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// SetScript replaces the script.
func (s *Server) SetScript(sc *Script) {
	s.mu.Lock()
	s.script = sc
	s.mu.Unlock()
}

// Requests returns the request log.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.log...)
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && path == "/_mock/requests":
		writeJSON(w, s.Requests())
		return
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/models"):
		writeJSON(w, map[string]any{"object": "list", "data": []any{}, "models": []any{}})
		return
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/messages/count_tokens"):
		writeJSON(w, map[string]int{"input_tokens": 10})
		return
	case r.Method != http.MethodPost:
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var api string
	var conv *conversation
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(body, &req)
	switch {
	case strings.HasSuffix(path, "/messages"):
		api = "anthropic"
		conv, err = parseAnthropic(body)
	case strings.HasSuffix(path, "/responses"):
		api = "responses"
		conv, err = parseResponses(body)
		req.Stream = req.Stream || strings.Contains(r.Header.Get("Accept"), "text/event-stream")
	case strings.HasSuffix(path, "/chat/completions"):
		api = "chat"
		conv, err = parseChat(body)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.n++
	n := s.n
	turn, step, idx := s.script.pick(conv)
	def := s.script.defaultText()
	s.mu.Unlock()
	rep := buildReply(api, conv, step, def, n)
	if step != nil && step.Delay > 0 {
		select {
		case <-time.After(time.Duration(step.Delay)):
		case <-r.Context().Done():
			return
		}
	}
	s.mu.Lock()
	s.log = append(s.log, Request{N: n, API: api, Path: path, Model: req.Model, Turn: turn, Step: idx, Reply: rep.String(), Body: string(body)})
	s.mu.Unlock()
	s.Logger.Info("model request", "n", n, "api", api, "turn", turn, "step", idx, "reply", rep.String())
	switch api {
	case "anthropic":
		if req.Stream {
			sse(w, anthropicEvents(rep, req.Model))
		} else {
			writeJSON(w, anthropicMessage(rep, req.Model))
		}
	case "responses":
		if req.Stream {
			sse(w, responsesEvents(rep, req.Model))
		} else {
			writeJSON(w, responsesObject(rep, req.Model))
		}
	case "chat":
		if req.Stream {
			sse(w, chatEvents(rep, req.Model))
		} else {
			writeJSON(w, chatObject(rep, req.Model))
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// event is one SSE event; an empty name writes only data.
type event struct {
	name string
	data any
}

func sse(w http.ResponseWriter, evs []event) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encodeSSE(evs))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func encodeSSE(evs []event) []byte {
	var b strings.Builder
	for _, e := range evs {
		if e.name != "" {
			fmt.Fprintf(&b, "event: %s\n", e.name)
		}
		if s, ok := e.data.(string); ok {
			fmt.Fprintf(&b, "data: %s\n\n", s)
			continue
		}
		d, _ := json.Marshal(e.data)
		fmt.Fprintf(&b, "data: %s\n\n", d)
	}
	return []byte(b.String())
}

// reply is a normalized model response.
type reply struct {
	n    int
	text string
	call *call
}

type call struct {
	name   string
	input  map[string]any // function arguments
	custom string         // freeform input (Responses custom tools)
	isCust bool
}

func (r reply) String() string {
	if r.call != nil {
		if r.call.isCust {
			return "tool " + r.call.name + " " + r.call.custom
		}
		b, _ := json.Marshal(r.call.input)
		return "tool " + r.call.name + " " + string(b)
	}
	return r.text
}

func (r reply) args() string {
	b, _ := json.Marshal(r.call.input)
	return string(b)
}

func buildReply(api string, c *conversation, st *Step, def string, n int) reply {
	if st == nil {
		return reply{n: n, text: def}
	}
	r := reply{n: n, text: st.Text}
	claude := api == "anthropic"
	switch {
	case st.Tool != nil:
		r.call = &call{name: st.Tool.Name, input: st.Tool.Input}
	case st.Command != "":
		r.call = commandCall(c, claude, st.Command)
	case st.Write != nil:
		if claude {
			r.call = &call{name: "Write", input: map[string]any{"file_path": st.Write.Path, "content": st.Write.Content}}
		} else if t, ok := c.hasTool("apply_patch"); ok {
			patch := "*** Begin Patch\n*** Add File: " + st.Write.Path + "\n"
			for _, l := range strings.SplitAfter(strings.TrimSuffix(st.Write.Content, "\n"), "\n") {
				patch += "+" + strings.TrimSuffix(l, "\n") + "\n"
			}
			patch += "*** End Patch\n"
			if t.custom {
				r.call = &call{name: "apply_patch", custom: patch, isCust: true}
			} else {
				r.call = &call{name: "apply_patch", input: map[string]any{"input": patch}}
			}
		} else {
			r.call = commandCall(c, false, "cat > "+shellQuote(st.Write.Path)+" <<'EOF'\n"+st.Write.Content+"\nEOF")
		}
	case st.MCP != nil:
		r.call = &call{name: mcpName(c, st.MCP.Server, st.MCP.Tool), input: st.MCP.Input}
	}
	if r.call != nil && r.call.input == nil && !r.call.isCust {
		r.call.input = map[string]any{}
	}
	if r.call == nil && r.text == "" {
		r.text = def
	}
	return r
}

func commandCall(c *conversation, claude bool, cmd string) *call {
	switch {
	case claude:
		return &call{name: "Bash", input: map[string]any{"command": cmd, "description": "scripted command"}}
	case has(c, "exec_command"):
		return &call{name: "exec_command", input: map[string]any{"cmd": cmd}}
	case has(c, "shell_command"):
		return &call{name: "shell_command", input: map[string]any{"command": cmd}}
	case has(c, "shell"):
		return &call{name: "shell", input: map[string]any{"command": []string{"bash", "-lc", cmd}}}
	default:
		return &call{name: "exec_command", input: map[string]any{"cmd": cmd}}
	}
}

func has(c *conversation, name string) bool { _, ok := c.hasTool(name); return ok }

func mcpName(c *conversation, server, tool string) string {
	want := "mcp__" + server + "__" + tool
	if has(c, want) {
		return want
	}
	for _, t := range c.tools {
		if strings.Contains(t.name, server) && strings.HasSuffix(t.name, tool) {
			return t.name
		}
	}
	return want
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

var usage = map[string]int{"input_tokens": 10, "output_tokens": 5}
