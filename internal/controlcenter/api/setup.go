package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"connectrpc.com/connect"

	ccv1 "github.com/usr-lex/swarmageddon/gen/swarm/controlcenter/v1"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/auth"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/auth/local"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/auth/session"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/k8s"
	"github.com/usr-lex/swarmageddon/internal/controlcenter/store"
)

type setupSvc struct{ *Server }

func (s setupSvc) GetSetupStatus(ctx context.Context, _ *connect.Request[ccv1.GetSetupStatusRequest]) (*connect.Response[ccv1.GetSetupStatusResponse], error) {
	out := &ccv1.GetSetupStatusResponse{DatabaseReady: s.DB() != nil}
	if p, err := s.K.Platform(ctx); err == nil {
		out.AdminClaimed = p.Status.AdminClaimed
		out.CaFingerprint = p.Status.CAFingerprint
		out.Url = p.Status.URL
	} else if !isNotFound(err) {
		return nil, kerr(err)
	}
	return connect.NewResponse(out), nil
}

// ClaimAdmin implements §3.3: the setup token, compared in constant time,
// rate-limited per client IP, every attempt audited.
func (s setupSvc) ClaimAdmin(ctx context.Context, req *connect.Request[ccv1.ClaimAdminRequest]) (*connect.Response[ccv1.ClaimAdminResponse], error) {
	ip := clientIP(ctx, req.Header(), req.Peer().Addr)
	attempt := func(result string) {
		s.Audit.Record(ctx, store.AuditEvent{Actor: "setup", Action: "setup.claim", SourceIP: ip, Result: result,
			Detail: fmt.Sprintf(`{"username":%q}`, req.Msg.GetUsername())})
	}
	if ok, wait := s.Setup.Allow(ip); !ok {
		attempt("rate limited")
		return nil, connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("too many attempts; try again in %s", wait.Round(1e9)))
	}
	if p, err := s.K.Platform(ctx); err == nil && p.Status.AdminClaimed {
		attempt("already claimed")
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("admin is already claimed"))
	}
	data, err := s.K.GetSecret(ctx, k8s.SecretSetupToken)
	if err != nil {
		attempt("no setup token")
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("there is no setup token; see the install output"))
	}
	want := strings.TrimSpace(string(data["token"]))
	got := strings.TrimSpace(req.Msg.GetToken())
	if want == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		attempt("wrong token")
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("wrong setup token"))
	}
	sd := session.Data{Provider: "setup", Subject: "setup", Handle: "setup", Role: auth.RoleAdmin.String()}
	if u := strings.TrimSpace(req.Msg.GetUsername()); u != "" {
		if store.HandleFrom(u) != strings.ToLower(u) {
			return nil, errInvalid("the username may contain only lower case letters, digits and dashes")
		}
		if err := local.CheckNew(req.Msg.GetPassword()); err != nil {
			return nil, errInvalid("%v", err)
		}
		hash, err := local.Hash(req.Msg.GetPassword())
		if err != nil {
			return nil, err
		}
		if err := s.K.ApplySecret(ctx, k8s.SecretLocalAdmin, map[string][]byte{
			"username": []byte(u), "passwordHash": []byte(hash), "email": []byte(req.Msg.GetEmail()),
		}); err != nil {
			attempt("saving the local admin failed: " + err.Error())
			return nil, kerr(err)
		}
		sd = session.Data{Provider: "local", Subject: u, Handle: u, Email: req.Msg.GetEmail(), Role: auth.RoleAdmin.String()}
	}
	if err := s.K.SetAdminClaimed(ctx); err != nil {
		attempt("recording adminClaimed failed: " + err.Error())
		return nil, kerr(err)
	}
	s.Setup.Reset(ip)
	attempt("ok")
	res := connect.NewResponse(&ccv1.ClaimAdminResponse{})
	p, err := s.startSession(ctx, cookieWriter{res.Header()}, requestFor(req), sd)
	if err != nil {
		return nil, err
	}
	res.Msg.User = userProto(p)
	return res, nil
}

// startSession records the sign-in (in the database when there is one) and
// sets the session cookie.
func (s *Server) startSession(ctx context.Context, w http.ResponseWriter, r *http.Request, sd session.Data) (*auth.Principal, error) {
	p := &auth.Principal{Handle: sd.Handle, Email: sd.Email, DisplayName: sd.DisplayName, Role: auth.ParseRole(sd.Role), Provider: sd.Provider}
	if db := s.DB(); db != nil {
		u, err := db.SignIn(ctx, store.Identity{Provider: sd.Provider, Subject: sd.Subject, Handle: sd.Handle, Email: sd.Email,
			DisplayName: sd.DisplayName, Groups: sd.Groups, MappedRole: sd.Role})
		if err != nil {
			return nil, err
		}
		if u.Disabled {
			return nil, connect.NewError(connect.CodePermissionDenied, errors.New("this account is disabled"))
		}
		sd.UserID = u.ID
		ws, _ := db.Assignments(ctx, u.ID)
		p = &auth.Principal{UserID: u.ID, Handle: u.Handle, Email: u.Email, DisplayName: u.DisplayName,
			Role: auth.ParseRole(u.Role()), Provider: u.Provider, Workspaces: ws}
		if p.Role == auth.RoleNone {
			return nil, connect.NewError(connect.CodePermissionDenied, errors.New("your account has no role here"))
		}
	}
	if _, err := s.Sessions.Create(ctx, w, r, sd); err != nil {
		return nil, err
	}
	return p, nil
}

// Promote moves a stateless session into the database (session.Manager.Promote).
func (s *Server) Promote(ctx context.Context, d *session.Data) (string, error) {
	db := s.DB()
	if db == nil {
		return "", errors.New("no database")
	}
	u, err := db.SignIn(ctx, store.Identity{Provider: d.Provider, Subject: d.Subject, Handle: d.Handle, Email: d.Email,
		DisplayName: d.DisplayName, Groups: d.Groups, MappedRole: d.Role})
	if err != nil {
		return "", err
	}
	return u.ID, nil
}
