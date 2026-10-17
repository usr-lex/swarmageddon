package telemetry

import (
	"context"
	"net/http"

	"connectrpc.com/connect"
	"github.com/getsentry/sentry-go"
	sentryhttp "github.com/getsentry/sentry-go/http"
)

// Handler traces h's requests and reports its panics (sentryhttp), when
// reporting is on. A panic is raised again afterwards, so net/http handles
// it as before. Without reporting it returns h.
func Handler(h http.Handler) http.Handler {
	if !Enabled() {
		return h
	}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tx := sentry.TransactionFromContext(r.Context()); tx != nil && (r.Method == http.MethodConnect || r.URL.IsAbs()) {
			// A proxy request: named by its method, not its target.
			tx.Name, tx.Source = r.Method+" (proxy)", sentry.SourceCustom
		}
		// sentryhttp renames the transaction after the route pattern a
		// ServeMux sets on its request. A copy keeps the name the URL path,
		// which for Connect is the procedure rather than its service.
		h.ServeHTTP(w, r.WithContext(r.Context()))
	})
	return sentryhttp.New(sentryhttp.Options{Repanic: true}).Handle(inner)
}

// Connect returns an interceptor for Connect handlers that reports server
// errors (codes unknown, internal and data loss) as events, one issue per
// procedure and code. With reporting off it does nothing.
func Connect() connect.Interceptor { return interceptor{} }

type interceptor struct{}

func (interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		res, err := next(ctx, req)
		rpcError(ctx, req.Spec().Procedure, err)
		return res, err
	}
}

func (interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		err := next(ctx, conn)
		rpcError(ctx, conn.Spec().Procedure, err)
		return err
	}
}

func rpcError(ctx context.Context, procedure string, err error) {
	if err == nil || !Enabled() {
		return
	}
	code := connect.CodeOf(err)
	if code != connect.CodeUnknown && code != connect.CodeInternal && code != connect.CodeDataLoss {
		return
	}
	h := hub(ctx)
	ev := h.Client().EventFromException(err, sentry.LevelError)
	// The stack would be the interceptor's, the same for every RPC.
	if n := len(ev.Exception); n > 0 && sentry.ExtractStacktrace(err) == nil {
		ev.Exception[n-1].Stacktrace = nil
	}
	ev.Tags = map[string]string{"rpc.procedure": procedure, "rpc.code": code.String()}
	ev.Fingerprint = []string{"connect", procedure, code.String()}
	h.CaptureEvent(ev)
}
