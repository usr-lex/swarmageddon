// Package msgclient connects a room to the messaging service: an mTLS
// Connect client that reloads rotated certificates, and the Subscribe loop
// with reconnects, at-least-once handling and an on-disk dedup set.
package msgclient

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	messagingv1 "github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1"
	"github.com/usr-lex/swarmageddon/gen/swarm/messaging/v1/messagingv1connect"
)

// TLSFiles are the room's client certificate, key and the platform CA.
type TLSFiles struct {
	Cert, Key, CA string
}

// reloader caches a keypair and CA pool, reloading when a file's mtime changes
// (kubelet replaces Secret files in place on rotation).
type reloader struct {
	files TLSFiles
	mu    sync.Mutex
	stamp string
	cert  *tls.Certificate
	pool  *x509.CertPool
}

func (r *reloader) load() (*tls.Certificate, *x509.CertPool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var stamp strings.Builder
	for _, f := range []string{r.files.Cert, r.files.Key, r.files.CA} {
		st, err := os.Stat(f)
		if err != nil {
			return nil, nil, err
		}
		fmt.Fprintf(&stamp, "%s:%d:%d;", f, st.ModTime().UnixNano(), st.Size())
	}
	if stamp.String() == r.stamp && r.cert != nil {
		return r.cert, r.pool, nil
	}
	cert, err := tls.LoadX509KeyPair(r.files.Cert, r.files.Key)
	if err != nil {
		return nil, nil, err
	}
	ca, err := os.ReadFile(r.files.CA)
	if err != nil {
		return nil, nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, nil, errors.New("no CA certificates in " + r.files.CA)
	}
	r.stamp, r.cert, r.pool = stamp.String(), &cert, pool
	return r.cert, r.pool, nil
}

// TLSConfig returns a client config that presents the current certificate
// and verifies the server against the current CA, both reloaded on change.
func TLSConfig(files TLSFiles) (*tls.Config, error) {
	r := &reloader{files: files}
	if _, _, err := r.load(); err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			c, _, err := r.load()
			return c, err
		},
		// Verification is done in VerifyConnection with the reloaded pool.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			_, pool, err := r.load()
			if err != nil {
				return err
			}
			if len(cs.PeerCertificates) == 0 {
				return errors.New("server sent no certificate")
			}
			inter := x509.NewCertPool()
			for _, c := range cs.PeerCertificates[1:] {
				inter.AddCert(c)
			}
			_, err = cs.PeerCertificates[0].Verify(x509.VerifyOptions{
				Roots: pool, Intermediates: inter, DNSName: cs.ServerName,
				KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			})
			return err
		},
	}, nil
}

// New returns a messaging client for baseURL (https://messaging...:8443).
// With empty files it uses plain HTTP (tests and dev).
func New(baseURL string, files TLSFiles) (messagingv1connect.MessagingServiceClient, error) {
	tr := &http.Transport{ForceAttemptHTTP2: true, IdleConnTimeout: 90 * time.Second}
	if files.Cert != "" {
		cfg, err := TLSConfig(files)
		if err != nil {
			return nil, err
		}
		tr.TLSClientConfig = cfg
	}
	return messagingv1connect.NewMessagingServiceClient(&http.Client{Transport: tr}, strings.TrimRight(baseURL, "/")), nil
}

// Dedup remembers the last Max delivered message IDs on disk.
type Dedup struct {
	Path string
	Max  int

	mu   sync.Mutex
	ids  []string
	seen map[string]bool
}

// LoadDedup reads the set from path (missing is empty).
func LoadDedup(path string, max int) (*Dedup, error) {
	d := &Dedup{Path: path, Max: max, seen: map[string]bool{}}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return d, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if id := strings.TrimSpace(sc.Text()); id != "" {
			d.add(id)
		}
	}
	return d, sc.Err()
}

func (d *Dedup) add(id string) {
	if d.seen[id] {
		return
	}
	d.seen[id] = true
	d.ids = append(d.ids, id)
	if len(d.ids) > d.Max {
		for _, old := range d.ids[:len(d.ids)-d.Max] {
			delete(d.seen, old)
		}
		d.ids = append([]string(nil), d.ids[len(d.ids)-d.Max:]...)
	}
}

// Seen reports whether id was delivered.
func (d *Dedup) Seen(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.seen[id]
}

// Add records id and persists the set (append, compacting when it doubles).
func (d *Dedup) Add(id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.seen[id] {
		return nil
	}
	d.add(id)
	if err := os.MkdirAll(filepath.Dir(d.Path), 0o700); err != nil {
		return err
	}
	if st, err := os.Stat(d.Path); err == nil && st.Size() > int64(d.Max)*2*40 {
		tmp := d.Path + ".tmp"
		if err := os.WriteFile(tmp, []byte(strings.Join(d.ids, "\n")+"\n"), 0o600); err != nil {
			return err
		}
		return os.Rename(tmp, d.Path)
	}
	f, err := os.OpenFile(d.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(id + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Handler processes one delivery; nil means it may be acked.
type Handler func(ctx context.Context, d *messagingv1.Delivery) error

// Subscribe keeps a Subscribe stream open (reconnecting with backoff) and
// passes each delivery to h, retrying h with backoff until it succeeds, then
// acks. Messages and notices already in dedup are acked without h.
func Subscribe(ctx context.Context, c messagingv1connect.MessagingServiceClient, dedup *Dedup, h Handler, log *slog.Logger) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := subscribeOnce(ctx, c, dedup, h, log)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		log.Warn("messaging stream ended; reconnecting", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func deliveryKey(d *messagingv1.Delivery) string {
	switch {
	case d.GetMessage() != nil:
		return "m:" + d.GetMessage().GetId()
	case d.GetNotice() != nil:
		return "n:" + d.GetNotice().GetId()
	}
	return ""
}

func subscribeOnce(ctx context.Context, c messagingv1connect.MessagingServiceClient, dedup *Dedup, h Handler, log *slog.Logger) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := c.Subscribe(ctx, connect.NewRequest(&messagingv1.SubscribeRequest{}))
	if err != nil {
		return err
	}
	defer stream.Close()
	log.Info("subscribed to messaging")
	for stream.Receive() {
		d := stream.Msg()
		key := deliveryKey(d)
		if key == "" || dedup == nil || !dedup.Seen(key) {
			backoff := time.Second
			for {
				err := h(ctx, d)
				if err == nil {
					break
				}
				log.Warn("delivering to the agent failed; retrying", "delivery", d.GetDeliveryId(), "err", err, "in", backoff)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(backoff):
				}
				backoff = min(backoff*2, 30*time.Second)
			}
			if key != "" && dedup != nil {
				if err := dedup.Add(key); err != nil {
					log.Warn("dedup set", "err", err)
				}
			}
		}
		if _, err := c.Ack(ctx, connect.NewRequest(&messagingv1.AckRequest{DeliveryId: d.GetDeliveryId()})); err != nil {
			return fmt.Errorf("ack: %w", err)
		}
	}
	if err := stream.Err(); err != nil {
		return err
	}
	return errors.New("stream closed by server")
}
