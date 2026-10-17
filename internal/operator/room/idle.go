package room

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"

	"connectrpc.com/connect"
	"sigs.k8s.io/controller-runtime/pkg/log"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1/messagingv1connect"
	"github.com/usr-lex/swarmageddon/internal/operator/deps"
	"github.com/usr-lex/swarmageddon/internal/pki"
)

// IdleAfter is how long a room must be idle before it may be rolled (§3.5).
const IdleAfter = 2 * time.Minute

// OperatorCN is the subject of the operator's messaging client certificate.
// Messaging lets it watch presence and approvals, nothing else.
const OperatorCN = "operator"

const operatorCertTTL = 24 * time.Hour

// pendingFor bounds how long an unanswered approval keeps a room busy: the
// default approval timeout, after which messaging drops the request without
// an event.
const pendingFor = 24 * time.Hour

type approval struct {
	room string
	at   time.Time
}

// PresenceSource is the part of the messaging API the idle checker uses.
type PresenceSource interface {
	WatchPresence(context.Context, *connect.Request[messagingv1.WatchPresenceRequest]) (*connect.ServerStreamForClient[messagingv1.PresenceEvent], error)
	WatchApprovals(context.Context, *connect.Request[messagingv1.WatchApprovalsRequest]) (*connect.ServerStreamForClient[messagingv1.ApprovalEvent], error)
}

// PresenceIdle is the IdleChecker over messaging (design §3.5): a room is
// idle once its presence has been idle for IdleAfter and none of its
// approvals is waiting. It keeps a presence watch and an approvals watch
// open; while either is down, no room counts as idle, and the idle clocks
// start again when the presence watch comes back. It runs as a manager
// Runnable.
type PresenceIdle struct {
	Source PresenceSource
	Now    func() time.Time

	mu        sync.Mutex
	idleSince map[string]time.Time // room address -> first seen idle
	pending   map[string]approval  // by approval ID
	presUp    bool
	apprSince time.Time // when the approvals watch connected; zero while down
}

// NewPresenceIdle returns an idle checker that talks to messaging at
// baseURL with a CN=operator client certificate from ca, issued in memory
// and renewed before it expires.
func NewPresenceIdle(ca deps.PKI, baseURL string) (*PresenceIdle, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CABundle()) {
		return nil, errors.New("no CA certificate")
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	certs := &operatorCert{ca: ca}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, ServerName: u.Hostname(),
			GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return certs.get(time.Now()) }},
		ForceAttemptHTTP2: true,
		IdleConnTimeout:   90 * time.Second,
	}
	c := messagingv1connect.NewMessagingServiceClient(&http.Client{Transport: tr}, baseURL, connect.WithGRPC())
	return &PresenceIdle{Source: c}, nil
}

// operatorCert issues the operator's client certificate and renews it at
// 2/3 of its life.
type operatorCert struct {
	ca      deps.PKI
	mu      sync.Mutex
	cert    *tls.Certificate
	renewAt time.Time
}

func (o *operatorCert) get(now time.Time) (*tls.Certificate, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.cert != nil && now.Before(o.renewAt) {
		return o.cert, nil
	}
	crtPEM, keyPEM, err := o.ca.IssueClientCert(OperatorCN, "spiffe://swarmageddon/system/operator", operatorCertTTL)
	if err != nil {
		return nil, err
	}
	c, err := tls.X509KeyPair(crtPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	leaf, err := pki.ParseCert(crtPEM)
	if err != nil {
		return nil, err
	}
	o.cert, o.renewAt = &c, pki.RenewAt(leaf)
	return o.cert, nil
}

func (p *PresenceIdle) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Idle implements IdleChecker.
func (p *PresenceIdle) Idle(_ context.Context, workspace, room string) (bool, error) {
	addr := room + "@" + workspace
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if !p.presUp || p.apprSince.IsZero() || now.Sub(p.apprSince) < IdleAfter {
		return false, nil
	}
	for _, a := range p.pending {
		if a.room == addr && now.Sub(a.at) < pendingFor {
			return false, nil
		}
	}
	since, ok := p.idleSince[addr]
	return ok && now.Sub(since) >= IdleAfter, nil
}

// Start keeps both watches open until ctx ends.
func (p *PresenceIdle) Start(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); loop(ctx, "presence", p.watchPresence) }()
	go func() { defer wg.Done(); loop(ctx, "approvals", p.watchApprovals) }()
	wg.Wait()
	return nil
}

// loop runs fn until ctx ends, restarting it with backoff.
func loop(ctx context.Context, name string, fn func(context.Context) error) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := fn(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		log.FromContext(ctx).Info("messaging watch ended; reconnecting", "watch", name, "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, 30*time.Second)
	}
}

func (p *PresenceIdle) watchPresence(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s, err := p.Source.WatchPresence(ctx, connect.NewRequest(&messagingv1.WatchPresenceRequest{}))
	if err != nil {
		return err
	}
	defer s.Close()
	p.setPresenceUp(true)
	defer p.setPresenceUp(false)
	for s.Receive() {
		p.presence(s.Msg())
	}
	return s.Err()
}

func (p *PresenceIdle) watchApprovals(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s, err := p.Source.WatchApprovals(ctx, connect.NewRequest(&messagingv1.WatchApprovalsRequest{}))
	if err != nil {
		return err
	}
	defer s.Close()
	p.setApprovalsUp(true)
	defer p.setApprovalsUp(false)
	for s.Receive() {
		p.approval(s.Msg())
	}
	return s.Err()
}

// setPresenceUp marks the presence watch up or down; either way the idle
// clocks start over, since changes may have been missed.
func (p *PresenceIdle) setPresenceUp(up bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.presUp = up
	p.idleSince = map[string]time.Time{}
}

// setApprovalsUp marks the approvals watch up or down. A new watch replays
// the pending approvals first, so the set starts empty.
func (p *PresenceIdle) setApprovalsUp(up bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pending = map[string]approval{}
	p.apprSince = time.Time{}
	if up {
		p.apprSince = p.now()
	}
}

// presence handles one presence event. Heartbeats repeat the current state;
// only a change from idle resets the room's idle clock.
func (p *PresenceIdle) presence(ev *messagingv1.PresenceEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.idleSince == nil {
		p.idleSince = map[string]time.Time{}
	}
	if ev.GetPresence() != messagingv1.Presence_PRESENCE_IDLE {
		delete(p.idleSince, ev.GetAddress())
		return
	}
	if _, ok := p.idleSince[ev.GetAddress()]; !ok {
		p.idleSince[ev.GetAddress()] = p.now()
	}
}

// approval tracks which rooms have approvals waiting.
func (p *PresenceIdle) approval(ev *messagingv1.ApprovalEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending == nil {
		p.pending = map[string]approval{}
	}
	switch e := ev.GetEvent().(type) {
	case *messagingv1.ApprovalEvent_Requested:
		at := p.now()
		if t := e.Requested.GetRequestedAt(); t != nil {
			at = t.AsTime()
		}
		p.pending[e.Requested.GetId()] = approval{room: e.Requested.GetRoom(), at: at}
	case *messagingv1.ApprovalEvent_Answered:
		delete(p.pending, e.Answered.GetApprovalId())
	case *messagingv1.ApprovalEvent_Withdrawn:
		delete(p.pending, e.Withdrawn.GetApprovalId())
	}
}
