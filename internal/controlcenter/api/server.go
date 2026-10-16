// Package api is the control center's HTTP server (docs/design.md §8): the
// Connect API under /api, sign-in redirects under /auth, terminals under
// /ws, the Forgejo webhook under /hooks/forgejo, health checks and the
// embedded UI with SPA fallback.
package api

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"

	"github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1/controlcenterv1connect"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/approvals"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/audit"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/auth"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/auth/session"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/forgehook"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/k8s"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/msgclient"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/notify"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/observatory"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/spend"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/store"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/terminal"
	"github.com/usr-lex/swarmageddon/web"
)

// Config holds the server's settings.
type Config struct {
	// PublicURL is the control center's external base URL, used for sign-in
	// redirects and SAML metadata. Empty: Platform status URL, else the request's host.
	PublicURL string
	Dev       bool
	ForgeURL  string // Forgejo's in-cluster URL, for permission checks
}

// Server holds every component the API uses.
type Server struct {
	Cfg       Config
	K         *k8s.Client
	Sessions  *session.Manager
	Audit     *audit.Auditor
	Setup     *auth.Limiter // setup token attempts
	Logins    *auth.Limiter // local password attempts
	Msg       *msgclient.Client
	Approvals *approvals.Manager
	Notify    *notify.Dispatcher
	Tracker   *notify.Tracker
	Spend     *spend.Poller
	Rec       *observatory.Recorder
	Presence  *observatory.Presence
	Fleet     *Fleet
	Inbox     *InboxHub
	Verdicts  *VerdictHub
	Exec      terminal.Execer
	Fwd       terminal.Forwarder
	Log       *slog.Logger

	db   atomic.Pointer[store.Store]
	idp  idpCache
	saml memSAMLRequests
}

// DB returns the store, or nil before PostgreSQL is connected.
func (s *Server) DB() *store.Store { return s.db.Load() }

// SetDB installs the store once connected.
func (s *Server) SetDB(db *store.Store) { s.db.Store(db) }

// Handler builds the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	opts := []connect.HandlerOption{connect.WithInterceptors(errorLogger(s.Log))}
	add := func(p string, h http.Handler) { mux.Handle("/api"+p, http.StripPrefix("/api", h)) }
	add(controlcenterv1connect.NewSetupServiceHandler(setupSvc{s}, opts...))
	add(controlcenterv1connect.NewAuthServiceHandler(authSvc{s}, opts...))
	add(controlcenterv1connect.NewPlatformServiceHandler(platformSvc{s}, opts...))
	add(controlcenterv1connect.NewWorkspaceServiceHandler(workspaceSvc{s}, opts...))
	add(controlcenterv1connect.NewRoomServiceHandler(roomSvc{s}, opts...))
	add(controlcenterv1connect.NewFleetServiceHandler(fleetSvc{s}, opts...))
	add(controlcenterv1connect.NewInboxServiceHandler(inboxSvc{s}, opts...))
	add(controlcenterv1connect.NewApprovalServiceHandler(approvalSvc{s}, opts...))
	add(controlcenterv1connect.NewAuditServiceHandler(auditSvc{s}, opts...))
	add(controlcenterv1connect.NewNotificationServiceHandler(notificationSvc{s}, opts...))
	add(controlcenterv1connect.NewSpendServiceHandler(spendSvc{s}, opts...))
	add(controlcenterv1connect.NewUserServiceHandler(userSvc{s}, opts...))
	add(controlcenterv1connect.NewObservatoryServiceHandler(observatorySvc{s}, opts...))
	add(controlcenterv1connect.NewChannelServiceHandler(channelSvc{s}, opts...))
	add(controlcenterv1connect.NewWardenServiceHandler(wardenSvc{s}, opts...))

	mux.HandleFunc("/auth/oidc/", s.handleOIDC)
	mux.HandleFunc("/auth/saml/", s.handleSAML)

	th := &terminal.Handler{
		Principal: func(w http.ResponseWriter, r *http.Request) *auth.Principal { return auth.FromContext(r.Context()) },
		Owners:    s.workspaceOwners,
		Exec:      s.Exec,
		Fwd:       s.Fwd,
		Audit:     s.Audit.Record,
	}
	mux.HandleFunc("/ws/terminal", th.ServeTerminal)
	mux.HandleFunc("/ws/vnc", th.ServeVNC)

	mux.Handle("/hooks/forgejo/", s.forgeHook())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", s.ready)
	mux.Handle("/", spa())
	return s.withPrincipal(s.csrf(securityHeaders(mux)))
}

// HealthHandler serves /healthz and /readyz on the health port.
func (s *Server) HealthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/readyz", s.ready)
	return mux
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if _, err := s.K.Platform(ctx); err != nil && !isNotFound(err) {
		http.Error(w, "kubernetes: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok"))
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "same-origin")
		hd.Set("X-Frame-Options", "DENY")
		h.ServeHTTP(w, r)
	})
}

// withPrincipal loads the session (refreshing it) and puts the principal in the context.
func (s *Server) withPrincipal(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/ws/") && !strings.HasPrefix(r.URL.Path, "/auth/") {
			h.ServeHTTP(w, r)
			return
		}
		if p := s.principal(w, r); p != nil {
			r = r.WithContext(auth.WithPrincipal(r.Context(), p))
		}
		h.ServeHTTP(w, r)
	})
}

func (s *Server) principal(w http.ResponseWriter, r *http.Request) *auth.Principal {
	d, err := s.Sessions.Load(r.Context(), w, r)
	if err != nil {
		return nil
	}
	if d.UserID == "" {
		return &auth.Principal{Handle: d.Handle, Email: d.Email, DisplayName: d.DisplayName, Role: auth.ParseRole(d.Role), Provider: d.Provider}
	}
	db := s.DB()
	if db == nil {
		return nil
	}
	u, err := db.GetUser(r.Context(), d.UserID)
	if err != nil || u.Disabled {
		return nil
	}
	ws, _ := db.Assignments(r.Context(), u.ID)
	return &auth.Principal{UserID: u.ID, Handle: u.Handle, Email: u.Email, DisplayName: u.DisplayName,
		Role: auth.ParseRole(u.Role()), Provider: u.Provider, Workspaces: ws}
}

// csrf protects state-changing API calls: every /api request must use the
// Connect or gRPC protocol (a custom header or content type no cross-site
// form can send without a CORS preflight, which we never allow), and an
// Origin header, when present, must be our own.
func (s *Server) csrf(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if r.Method == http.MethodOptions {
				http.Error(w, "cross-origin requests are not allowed", http.StatusForbidden)
				return
			}
			ct := r.Header.Get("Content-Type")
			connectProto := r.Header.Get("Connect-Protocol-Version") == "1" ||
				strings.HasPrefix(ct, "application/connect+") || strings.HasPrefix(ct, "application/grpc")
			if !connectProto {
				http.Error(w, "missing Connect-Protocol-Version header", http.StatusForbidden)
				return
			}
			if !s.sameOrigin(r) {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

func (s *Server) sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true // not a browser cross-origin request
	}
	u, err := url.Parse(o)
	if err != nil {
		return false
	}
	if u.Host == r.Host {
		return true
	}
	if s.Cfg.PublicURL != "" {
		if pu, err := url.Parse(s.Cfg.PublicURL); err == nil && pu.Host == u.Host {
			return true
		}
	}
	return false
}

// spa serves the embedded UI; unknown paths get index.html.
func spa() http.Handler {
	dist, built := web.Dist()
	files := http.FileServer(http.FS(dist))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !built {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(web.Placeholder())
			return
		}
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(dist, p); err != nil {
			if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/auth/") || path.Ext(p) != "" {
				http.NotFound(w, r)
				return
			}
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}
		if strings.HasPrefix(p, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

func errorLogger(log *slog.Logger) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			res, err := next(ctx, req)
			if err != nil {
				var ce *connect.Error
				if !errors.As(err, &ce) || ce.Code() == connect.CodeInternal || ce.Code() == connect.CodeUnknown {
					log.Warn("api error", "procedure", req.Spec().Procedure, "err", err)
				}
			}
			return res, err
		}
	})
}

func (s *Server) forgeHook() http.Handler {
	perms := &forgehook.ForgeReader{BaseURL: s.Cfg.ForgeURL, Token: func(ctx context.Context) (string, error) {
		d, err := s.K.GetSecret(ctx, k8s.SecretForgeReader)
		if err != nil {
			return "", err
		}
		return string(d["token"]), nil
	}}
	h := &forgehook.Handler{
		Dir:   forgeDir{s},
		Perms: perms,
		Send:  s.Msg,
		CIRed: func(ctx context.Context, ev notify.Event) { s.Notify.Raise(ctx, ev) },
		Audit: s.Audit.Record,
		Record: func(ctx context.Context, evs ...store.Activity) {
			s.Rec.Record(ctx, evs...)
		},
		Log: s.Log,
	}
	if s.Msg == nil {
		h.Send = noSender{}
	}
	return h
}

// workspaceOwners returns a Workspace's spec.owners.
func (s *Server) workspaceOwners(ctx context.Context, ws string) ([]string, error) {
	w, err := s.K.Workspace(ctx, ws)
	if err != nil {
		return nil, err
	}
	return w.Spec.Owners, nil
}

// publicURL is the external base URL.
func (s *Server) publicURL(r *http.Request) string {
	if s.Cfg.PublicURL != "" {
		return strings.TrimRight(s.Cfg.PublicURL, "/")
	}
	if p, err := s.K.Platform(r.Context()); err == nil && p.Status.URL != "" {
		return strings.TrimRight(p.Status.URL, "/")
	}
	scheme := "https"
	if s.Cfg.Dev && r.TLS == nil {
		scheme = "http"
	}
	return scheme + "://" + r.Host
}
