package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

// Paths and env the kubelet projects into every pod.
const (
	saTokenPath     = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	saCACertPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	saNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
)

// leaseSpec mirrors coordination.k8s.io/v1 LeaseSpec. Only the fields this
// service needs are modelled; unknown fields survive a round trip because
// the whole object is decoded, mutated, and sent back.
type leaseSpec struct {
	HolderIdentity       *string `json:"holderIdentity,omitempty"`
	LeaseDurationSeconds *int32  `json:"leaseDurationSeconds,omitempty"`
	AcquireTime          *string `json:"acquireTime,omitempty"`
	RenewTime            *string `json:"renewTime,omitempty"`
	LeaseTransitions     *int32  `json:"leaseTransitions,omitempty"`
}

type leaseObject struct {
	APIVersion string         `json:"apiVersion,omitempty"`
	Kind       string         `json:"kind,omitempty"`
	Metadata   leaseMeta      `json:"metadata"`
	Spec       leaseSpec      `json:"spec"`
	Extra      map[string]any `json:"-"`
}

type leaseMeta struct {
	Name            string `json:"name"`
	Namespace       string `json:"namespace"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
}

// errLeaseConflict means another pod wrote the lease first. The API server
// detects it by comparing resourceVersion, so mutual exclusion is enforced
// there rather than by agreement between pods.
var errLeaseConflict = errors.New("lease conflict")

// LeaseClient talks to the Kubernetes API with the projected ServiceAccount
// token. It exists instead of client-go because the clientset links every
// API group: importing it costs roughly 20 MB of binary and 77 modules to
// use one resource.
type LeaseClient struct {
	host      string
	namespace string
	name      string
	hc        *http.Client

	// tokenPath is re-read per request: projected tokens are rotated, and a
	// token cached at startup stops working after an hour or so.
	tokenPath string
}

func NewLeaseClient(cfg Config) (*LeaseClient, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("not running in a cluster: KUBERNETES_SERVICE_HOST/PORT unset")
	}

	ca, err := os.ReadFile(saCACertPath)
	if err != nil {
		return nil, fmt.Errorf("reading cluster CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("cluster CA is not valid PEM")
	}

	namespace := cfg.PodNamespace
	if b, err := os.ReadFile(saNamespacePath); err == nil && len(b) > 0 {
		namespace = string(bytes.TrimSpace(b))
	}

	return &LeaseClient{
		host:      "https://" + net.JoinHostPort(host, port),
		namespace: namespace,
		name:      cfg.LeaseName,
		tokenPath: saTokenPath,
		hc: &http.Client{
			Timeout:   10 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		},
	}, nil
}

func (c *LeaseClient) collectionPath() string {
	return fmt.Sprintf("/apis/coordination.k8s.io/v1/namespaces/%s/leases", c.namespace)
}

func (c *LeaseClient) objectPath() string {
	return c.collectionPath() + "/" + c.name
}

func (c *LeaseClient) do(ctx context.Context, method, path string, in, out any) (int, error) {
	var body io.Reader = http.NoBody
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.host+path, body)
	if err != nil {
		return 0, err
	}
	token, err := os.ReadFile(c.tokenPath)
	if err != nil {
		return 0, fmt.Errorf("reading service account token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+string(bytes.TrimSpace(token)))
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if out != nil && isHTTPSuccess(resp.StatusCode) {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
			return resp.StatusCode, err
		}
		return resp.StatusCode, nil
	}
	if !isHTTPSuccess(resp.StatusCode) && resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusConflict {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return resp.StatusCode, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(msg))
	}
	return resp.StatusCode, nil
}

// Get returns the lease, reporting ok=false when it does not exist yet.
func (c *LeaseClient) Get(ctx context.Context) (leaseObject, bool, error) {
	var l leaseObject
	code, err := c.do(ctx, http.MethodGet, c.objectPath(), nil, &l)
	if err != nil {
		return leaseObject{}, false, err
	}
	if code == http.StatusNotFound {
		return leaseObject{}, false, nil
	}
	return l, true, nil
}

// TryAcquire takes or renews the lease for identity, reporting whether this
// pod now holds it.
//
// The write carries the resourceVersion read a moment earlier, so the API
// server rejects it with 409 if another pod got there first. Two pods racing
// cannot both win.
func (c *LeaseClient) TryAcquire(ctx context.Context, identity string, ttl time.Duration, now time.Time) (bool, error) {
	stamp := now.UTC().Format(time.RFC3339Nano)
	secs := int32(ttl.Seconds())

	cur, exists, err := c.Get(ctx)
	if err != nil {
		return false, err
	}

	if !exists {
		l := leaseObject{
			APIVersion: "coordination.k8s.io/v1",
			Kind:       "Lease",
			Metadata:   leaseMeta{Name: c.name, Namespace: c.namespace},
			Spec: leaseSpec{
				HolderIdentity:       &identity,
				LeaseDurationSeconds: &secs,
				AcquireTime:          &stamp,
				RenewTime:            &stamp,
				LeaseTransitions:     ptrInt32(0),
			},
		}
		code, err := c.do(ctx, http.MethodPost, c.collectionPath(), l, nil)
		if err != nil {
			return false, err
		}
		if code == http.StatusConflict {
			// Another pod created it in the same instant.
			return false, nil
		}
		return isHTTPSuccess(code), nil
	}

	held := cur.Spec.HolderIdentity != nil && *cur.Spec.HolderIdentity == identity
	if !held && !leaseExpired(cur, now) {
		return false, nil
	}

	if !held {
		cur.Spec.AcquireTime = &stamp
		cur.Spec.LeaseTransitions = ptrInt32(deref(cur.Spec.LeaseTransitions) + 1)
	}
	cur.Spec.HolderIdentity = &identity
	cur.Spec.LeaseDurationSeconds = &secs
	cur.Spec.RenewTime = &stamp

	code, err := c.do(ctx, http.MethodPut, c.objectPath(), cur, nil)
	if err != nil {
		return false, err
	}
	if code == http.StatusConflict {
		return false, errLeaseConflict
	}
	return isHTTPSuccess(code), nil
}

// Release hands the lease back so a successor does not have to wait out the
// full duration. Best effort: a missed release costs one lease duration.
func (c *LeaseClient) Release(ctx context.Context, identity string) error {
	cur, exists, err := c.Get(ctx)
	if err != nil || !exists {
		return err
	}
	if cur.Spec.HolderIdentity == nil || *cur.Spec.HolderIdentity != identity {
		return nil
	}

	cur.Spec.HolderIdentity = ptrString("")
	cur.Spec.RenewTime = ptrString(time.Unix(0, 0).UTC().Format(time.RFC3339Nano))

	_, err = c.do(ctx, http.MethodPut, c.objectPath(), cur, nil)
	return err
}

// leaseExpired reports whether the current holder has stopped renewing.
func leaseExpired(l leaseObject, now time.Time) bool {
	if l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity == "" {
		return true
	}
	if l.Spec.RenewTime == nil {
		return true
	}
	renewed, err := time.Parse(time.RFC3339, *l.Spec.RenewTime)
	if err != nil {
		return true
	}
	ttl := time.Duration(deref(l.Spec.LeaseDurationSeconds)) * time.Second
	if ttl <= 0 {
		return true
	}
	return now.Sub(renewed) > ttl
}

// LeaseElector keeps this pod's primary status current. Losing the API
// server must never affect serving: it only stops this pod from pulling.
type LeaseElector struct {
	client   *LeaseClient
	identity string
	ttl      time.Duration
	renew    time.Duration
	logger   *slog.Logger

	mu          sync.RWMutex
	primary     bool
	holder      string
	lastSuccess time.Time
	lastErr     string

	now func() time.Time
}

func NewLeaseElector(client *LeaseClient, cfg Config, logger *slog.Logger) *LeaseElector {
	return &LeaseElector{
		client:   client,
		identity: cfg.PodName,
		ttl:      cfg.LeaseDuration,
		renew:    cfg.LeaseRenewInterval,
		logger:   logger,
		now:      time.Now,
	}
}

func (e *LeaseElector) Identity() string { return e.identity }

func (e *LeaseElector) IsPrimary() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.primary
}

func (e *LeaseElector) Primary() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.holder
}

// Health reports the last error and when contact with the API server last
// succeeded, for /monitor. Nothing here gates serving.
func (e *LeaseElector) Health() (lastSuccess time.Time, lastErr string) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.lastSuccess, e.lastErr
}

// Run maintains the lease until ctx ends, then releases it.
func (e *LeaseElector) Run(ctx context.Context) {
	ticker := time.NewTicker(e.renew)
	defer ticker.Stop()

	e.step(ctx)
	for {
		select {
		case <-ctx.Done():
			e.releaseOnShutdown()
			return
		case <-ticker.C:
			e.step(ctx)
		}
	}
}

func (e *LeaseElector) step(ctx context.Context) {
	now := e.now()

	acquired, err := e.client.TryAcquire(ctx, e.identity, e.ttl, now)
	if err != nil && !errors.Is(err, errLeaseConflict) {
		e.recordFailure(now, err)
		return
	}

	holder := e.identity
	if !acquired {
		// Not ours: report who does hold it, so /cluster can show it.
		if l, ok, getErr := e.client.Get(ctx); getErr == nil && ok && l.Spec.HolderIdentity != nil {
			holder = *l.Spec.HolderIdentity
		} else {
			holder = ""
		}
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.primary = acquired
	e.holder = holder
	e.lastSuccess = now
	e.lastErr = ""
}

// recordFailure steps down once the lease this pod holds could have expired.
// Continuing to act as primary while unable to renew is how two pods end up
// pulling at once.
func (e *LeaseElector) recordFailure(now time.Time, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.lastErr = err.Error()
	if e.primary && now.Sub(e.lastSuccess) > e.ttl {
		e.logger.Warn("lease: stepping down, cannot renew",
			slog.String("err", e.lastErr),
			slog.Duration("since_last_success", now.Sub(e.lastSuccess)),
		)
		e.primary = false
		e.holder = ""
	}
}

func (e *LeaseElector) releaseOnShutdown() {
	if !e.IsPrimary() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := e.client.Release(ctx, e.identity); err != nil {
		e.logger.Warn("lease: release failed", slog.String("err", err.Error()))
		return
	}
	e.logger.Info("lease: released")
}

func ptrInt32(v int32) *int32 { return &v }

func deref(p *int32) int32 {
	if p == nil {
		return 0
	}
	return *p
}
