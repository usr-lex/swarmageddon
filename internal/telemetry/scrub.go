package telemetry

import (
	"fmt"
	"strings"

	"github.com/getsentry/sentry-go"
	"github.com/getsentry/sentry-go/attribute"

	"github.com/usr-lex/swarmageddon/internal/redact"
)

// The SDK's hooks call these on everything before it is sent. They redact
// with the values registered at that moment: an event is scrubbed when it
// is captured, breadcrumbs included, so a secret registered after a
// breadcrumb was recorded is still caught; a log record is scrubbed when it
// is logged, like the console's. Maps and breadcrumbs may be shared with a
// scope that other goroutines read, so they are rebuilt, never changed in
// place.

// dropHeader reports whether a header is never sent: credentials, cookies,
// and our own x-swarm-* identity headers.
func dropHeader(name string) bool {
	switch name = strings.ToLower(name); name {
	case "authorization", "proxy-authorization", "cookie", "set-cookie", "x-api-key":
		return true
	}
	return strings.HasPrefix(name, "x-swarm-")
}

func str(s string) string { return redact.Default.String(s) }

func scrubEvent(e *sentry.Event, _ *sentry.EventHint) *sentry.Event {
	e.Message = str(e.Message)
	e.Transaction = str(e.Transaction)
	e.Fingerprint = strs(e.Fingerprint)
	for i := range e.Exception {
		e.Exception[i].Value = str(e.Exception[i].Value)
	}
	for i, b := range e.Breadcrumbs {
		e.Breadcrumbs[i] = scrubBreadcrumb(b, nil)
	}
	e.Tags = strMap(e.Tags)
	if e.Contexts != nil {
		cs := make(map[string]sentry.Context, len(e.Contexts))
		for k, c := range e.Contexts {
			cs[k] = anyMap(c)
		}
		e.Contexts = cs
	}
	e.User = sentry.User{ID: str(e.User.ID), Email: str(e.User.Email), Username: str(e.User.Username), Name: str(e.User.Name), Data: strMap(e.User.Data)}
	if e.Request != nil {
		e.Request = scrubRequest(e.Request)
	}
	for _, s := range e.Spans {
		scrubSpan(s)
	}
	return e
}

// scrubRequest drops the body, cookies and credential headers, and redacts
// the rest.
func scrubRequest(r *sentry.Request) *sentry.Request {
	c := *r
	c.URL, c.QueryString = str(r.URL), str(r.QueryString)
	c.Data, c.Cookies = "", ""
	c.Headers = map[string]string{}
	for k, v := range r.Headers {
		if !dropHeader(k) {
			c.Headers[k] = str(v)
		}
	}
	c.Env = strMap(r.Env)
	return &c
}

// scrubSpan redacts a finished span (nothing else writes to it any more),
// dropping the credential headers sentryhttpclient records as data.
func scrubSpan(s *sentry.Span) {
	s.Name, s.Op, s.Description = str(s.Name), str(s.Op), str(s.Description)
	s.Tags = strMap(s.Tags)
	if s.Data == nil {
		return
	}
	d := make(map[string]any, len(s.Data))
	for k, v := range s.Data {
		h, ok := strings.CutPrefix(k, "http.request.header.")
		if !ok {
			h, ok = strings.CutPrefix(k, "http.response.header.")
		}
		if ok && dropHeader(h) {
			continue
		}
		d[k] = scrubAny(v)
	}
	s.Data = d
}

func scrubBreadcrumb(b *sentry.Breadcrumb, _ *sentry.BreadcrumbHint) *sentry.Breadcrumb {
	if b == nil {
		return nil
	}
	c := *b
	c.Message = str(b.Message)
	c.Data = anyMap(b.Data)
	return &c
}

func scrubLog(l *sentry.Log) *sentry.Log {
	l.Body = str(l.Body)
	for k, v := range l.Attributes {
		switch v.Type() {
		case attribute.STRING:
			l.Attributes[k] = attribute.StringValue(str(v.AsString()))
		case attribute.STRINGSLICE:
			l.Attributes[k] = attribute.StringSliceValue(strs(v.AsStringSlice()))
		}
	}
	return l
}

func strs(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = str(s)
	}
	return out
}

func strMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = str(v)
	}
	return out
}

func anyMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = scrubAny(v)
	}
	return out
}

// scrubAny redacts strings inside v. Errors, Stringers and other values that
// print with a secret become their redacted text, as in redact.Handler.
func scrubAny(v any) any {
	switch x := v.(type) {
	case nil, bool, int, int64, uint64, float64:
		return v
	case string:
		return str(x)
	case []string:
		return strs(x)
	case map[string]string:
		return strMap(x)
	case map[string]any:
		return anyMap(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = scrubAny(e)
		}
		return out
	}
	var s string
	switch x := v.(type) {
	case error:
		s = x.Error()
	case fmt.Stringer:
		s = x.String()
	default:
		s = fmt.Sprint(x)
	}
	if r := str(s); r != s {
		return r
	}
	return v
}
