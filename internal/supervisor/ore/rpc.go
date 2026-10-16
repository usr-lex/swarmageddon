package ore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/coder/websocket"
)

// RPCError is a JSON-RPC error from the app-server.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("app-server error %d: %s", e.Code, e.Message) }

// message is any JSON-RPC message (the app-server omits "jsonrpc").
type message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

// Client is a JSON-RPC client over WebSocket framing on a Unix socket.
type Client struct {
	conn *websocket.Conn

	mu      sync.Mutex
	next    int64
	pending map[string]chan message

	// OnNotification and OnRequest are called from the read loop; they must
	// not block on Calls.
	OnNotification func(method string, params json.RawMessage)
	OnRequest      func(id json.RawMessage, method string, params json.RawMessage)
}

// Dial connects to the app-server's unix socket.
func Dial(ctx context.Context, socket string) (*Client, error) {
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}
	c, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{HTTPClient: hc})
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(64 << 20)
	return &Client{conn: c, pending: map[string]chan message{}}, nil
}

// Run reads messages until the connection ends.
func (c *Client) Run(ctx context.Context) error {
	defer func() {
		c.mu.Lock()
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.mu.Unlock()
	}()
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			return err
		}
		var m message
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		switch {
		case m.Method != "" && len(m.ID) > 0:
			if c.OnRequest != nil {
				c.OnRequest(m.ID, m.Method, m.Params)
			}
		case m.Method != "":
			if c.OnNotification != nil {
				c.OnNotification(m.Method, m.Params)
			}
		case len(m.ID) > 0:
			c.mu.Lock()
			ch := c.pending[string(m.ID)]
			delete(c.pending, string(m.ID))
			c.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		}
	}
}

// Close closes the connection.
func (c *Client) Close() error { return c.conn.Close(websocket.StatusNormalClosure, "") }

func (c *Client) write(ctx context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.conn.Write(ctx, websocket.MessageText, b)
}

// Call sends a request and decodes the result into out (if non-nil).
func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	c.mu.Lock()
	c.next++
	id := json.RawMessage(fmt.Sprint(c.next))
	ch := make(chan message, 1)
	c.pending[string(id)] = ch
	c.mu.Unlock()
	if err := c.write(ctx, map[string]any{"id": id, "method": method, "params": params}); err != nil {
		c.mu.Lock()
		delete(c.pending, string(id))
		c.mu.Unlock()
		return err
	}
	select {
	case m, ok := <-ch:
		if !ok {
			return errors.New("app-server connection closed")
		}
		if m.Error != nil {
			return m.Error
		}
		if out != nil {
			return json.Unmarshal(m.Result, out)
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, string(id))
		c.mu.Unlock()
		return ctx.Err()
	}
}

// Notify sends a notification.
func (c *Client) Notify(ctx context.Context, method string, params any) error {
	m := map[string]any{"method": method}
	if params != nil {
		m["params"] = params
	}
	return c.write(ctx, m)
}

// Respond answers a server request.
func (c *Client) Respond(ctx context.Context, id json.RawMessage, result any) error {
	return c.write(ctx, map[string]any{"id": id, "result": result})
}

// isNoActiveTurn reports a steer that raced the end of a turn.
func isNoActiveTurn(err error) bool {
	var re *RPCError
	if !errors.As(err, &re) {
		return false
	}
	msg := strings.ToLower(re.Message)
	return strings.Contains(msg, "no active turn") || strings.Contains(msg, "expected") || strings.Contains(msg, "mismatch")
}
