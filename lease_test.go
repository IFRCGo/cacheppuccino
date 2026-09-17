package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAPIServer emulates the slice of the Kubernetes API this client uses,
// including the compare-and-swap on resourceVersion that makes two pods
// unable to both win the lease.
type fakeAPIServer struct {
	mu       sync.Mutex
	lease    *leaseObject
	revision int
	puts     int
	conflict bool // force the next PUT to conflict
}

func (f *fakeAPIServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		switch {
		case r.Method == http.MethodGet:
			if f.lease == nil {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(f.lease)

		case r.Method == http.MethodPost:
			if f.lease != nil {
				http.Error(w, "already exists", http.StatusConflict)
				return
			}
			var l leaseObject
			if err := json.NewDecoder(r.Body).Decode(&l); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			f.revision++
			l.Metadata.ResourceVersion = strconv.Itoa(f.revision)
			f.lease = &l
			_ = json.NewEncoder(w).Encode(l)

		case r.Method == http.MethodPut:
			var l leaseObject
			if err := json.NewDecoder(r.Body).Decode(&l); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			f.puts++
			if f.conflict || f.lease == nil || l.Metadata.ResourceVersion != f.lease.Metadata.ResourceVersion {
				f.conflict = false
				http.Error(w, "the object has been modified", http.StatusConflict)
				return
			}
			f.revision++
			l.Metadata.ResourceVersion = strconv.Itoa(f.revision)
			f.lease = &l
			_ = json.NewEncoder(w).Encode(l)

		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	})
}

func (f *fakeAPIServer) holder() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lease == nil || f.lease.Spec.HolderIdentity == nil {
		return ""
	}
	return *f.lease.Spec.HolderIdentity
}

func (f *fakeAPIServer) transitions() int32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lease == nil {
		return 0
	}
	return deref(f.lease.Spec.LeaseTransitions)
}

// newTestLeaseClient points a client at the fake API server with a token
// file on disk, exercising the real per-request token read.
func newTestLeaseClient(t *testing.T, srv *httptest.Server) *LeaseClient {
	t.Helper()

	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("test-token\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	return &LeaseClient{
		host:      srv.URL,
		namespace: "test-ns",
		name:      "cacheppuccino",
		hc:        srv.Client(),
		tokenPath: tokenPath,
	}
}

func TestLeaseAcquireCreatesWhenAbsent(t *testing.T) {
	api := &fakeAPIServer{}
	srv := httptest.NewServer(api.handler())
	defer srv.Close()

	c := newTestLeaseClient(t, srv)

	ok, err := c.TryAcquire(context.Background(), "pod-a", 15*time.Second, time.Now())
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	if !ok {
		t.Fatalf("did not acquire a lease that did not exist")
	}
	if api.holder() != "pod-a" {
		t.Errorf("holder = %q, want pod-a", api.holder())
	}
}

func TestLeaseRenewKeepsHolderAndTransitions(t *testing.T) {
	api := &fakeAPIServer{}
	srv := httptest.NewServer(api.handler())
	defer srv.Close()

	c := newTestLeaseClient(t, srv)
	now := time.Now()

	if _, err := c.TryAcquire(context.Background(), "pod-a", 15*time.Second, now); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	ok, err := c.TryAcquire(context.Background(), "pod-a", 15*time.Second, now.Add(5*time.Second))
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if !ok {
		t.Errorf("holder failed to renew its own lease")
	}
	// Renewing is not a handover, so the transition count must not move.
	if got := api.transitions(); got != 0 {
		t.Errorf("leaseTransitions = %d, want 0 on renew", got)
	}
}

func TestLeaseDoesNotStealFromLiveHolder(t *testing.T) {
	api := &fakeAPIServer{}
	srv := httptest.NewServer(api.handler())
	defer srv.Close()

	c := newTestLeaseClient(t, srv)
	now := time.Now()

	if _, err := c.TryAcquire(context.Background(), "pod-a", 15*time.Second, now); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Well inside the lease duration: pod-b must back off.
	ok, err := c.TryAcquire(context.Background(), "pod-b", 15*time.Second, now.Add(5*time.Second))
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	if ok {
		t.Errorf("pod-b took a lease still being renewed by pod-a")
	}
	if api.holder() != "pod-a" {
		t.Errorf("holder = %q, want pod-a", api.holder())
	}
}

func TestLeaseTakesOverAfterHolderStopsRenewing(t *testing.T) {
	api := &fakeAPIServer{}
	srv := httptest.NewServer(api.handler())
	defer srv.Close()

	c := newTestLeaseClient(t, srv)
	now := time.Now()

	if _, err := c.TryAcquire(context.Background(), "pod-a", 15*time.Second, now); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Past the lease duration, which is what a dead primary looks like.
	ok, err := c.TryAcquire(context.Background(), "pod-b", 15*time.Second, now.Add(20*time.Second))
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	if !ok {
		t.Fatalf("pod-b did not take over an expired lease")
	}
	if api.holder() != "pod-b" {
		t.Errorf("holder = %q, want pod-b", api.holder())
	}
	if got := api.transitions(); got != 1 {
		t.Errorf("leaseTransitions = %d, want 1 after a handover", got)
	}
}

// The safety argument for the whole design is that the API server, not our
// code, decides who wins: a stale resourceVersion must be rejected.
func TestLeaseStaleResourceVersionConflicts(t *testing.T) {
	api := &fakeAPIServer{}
	srv := httptest.NewServer(api.handler())
	defer srv.Close()

	c := newTestLeaseClient(t, srv)
	now := time.Now()

	if _, err := c.TryAcquire(context.Background(), "pod-a", 15*time.Second, now); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	api.mu.Lock()
	api.conflict = true
	api.mu.Unlock()

	ok, err := c.TryAcquire(context.Background(), "pod-a", 15*time.Second, now.Add(time.Second))
	if ok {
		t.Errorf("reported success despite a conflict")
	}
	if !errors.Is(err, errLeaseConflict) {
		t.Errorf("err = %v, want errLeaseConflict", err)
	}
}

func TestLeaseReleaseClearsHolder(t *testing.T) {
	api := &fakeAPIServer{}
	srv := httptest.NewServer(api.handler())
	defer srv.Close()

	c := newTestLeaseClient(t, srv)
	if _, err := c.TryAcquire(context.Background(), "pod-a", 15*time.Second, time.Now()); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	if err := c.Release(context.Background(), "pod-a"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got := api.holder(); got != "" {
		t.Errorf("holder = %q, want cleared", got)
	}

	// A released lease is immediately available, so failover does not have
	// to wait out the full duration.
	ok, err := c.TryAcquire(context.Background(), "pod-b", 15*time.Second, time.Now())
	if err != nil || !ok {
		t.Errorf("successor could not take a released lease: ok=%v err=%v", ok, err)
	}
}

func TestLeaseReleaseIgnoresLeaseHeldByAnother(t *testing.T) {
	api := &fakeAPIServer{}
	srv := httptest.NewServer(api.handler())
	defer srv.Close()

	c := newTestLeaseClient(t, srv)
	if _, err := c.TryAcquire(context.Background(), "pod-a", 15*time.Second, time.Now()); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	if err := c.Release(context.Background(), "pod-b"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if api.holder() != "pod-a" {
		t.Errorf("holder = %q, want pod-a to survive another pod's release", api.holder())
	}
}

// Many pods contending at once must leave exactly one holder.
func TestLeaseConcurrentAcquireElectsOne(t *testing.T) {
	api := &fakeAPIServer{}
	srv := httptest.NewServer(api.handler())
	defer srv.Close()

	now := time.Now()
	var (
		mu      sync.Mutex
		winners []string
		wg      sync.WaitGroup
	)
	for i := range 8 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			c := newTestLeaseClientShared(t, srv)
			identity := "pod-" + strconv.Itoa(n)
			ok, err := c.TryAcquire(context.Background(), identity, 15*time.Second, now)
			if err != nil && !errors.Is(err, errLeaseConflict) {
				return
			}
			if ok {
				mu.Lock()
				winners = append(winners, identity)
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if len(winners) != 1 {
		t.Fatalf("winners = %v, want exactly one", winners)
	}
	if api.holder() != winners[0] {
		t.Errorf("holder = %q, want %q", api.holder(), winners[0])
	}
}

// newTestLeaseClientShared is newTestLeaseClient without t.TempDir, which is
// not safe to call from parallel goroutines.
func newTestLeaseClientShared(t *testing.T, srv *httptest.Server) *LeaseClient {
	return &LeaseClient{
		host:      srv.URL,
		namespace: "test-ns",
		name:      "cacheppuccino",
		hc:        srv.Client(),
		tokenPath: sharedTokenPath(t),
	}
}

var (
	sharedTokenOnce sync.Once
	sharedToken     string
)

func sharedTokenPath(t *testing.T) string {
	sharedTokenOnce.Do(func() {
		dir, err := os.MkdirTemp("", "lease-token")
		if err != nil {
			t.Fatalf("MkdirTemp: %v", err)
		}
		sharedToken = filepath.Join(dir, "token")
		if err := os.WriteFile(sharedToken, []byte("test-token"), 0o600); err != nil {
			t.Fatalf("write token: %v", err)
		}
	})
	return sharedToken
}

func TestElectorBecomesPrimaryAndReportsHolder(t *testing.T) {
	api := &fakeAPIServer{}
	srv := httptest.NewServer(api.handler())
	defer srv.Close()

	e := testElector(t, srv, "pod-a")
	e.step(context.Background())

	if !e.IsPrimary() {
		t.Fatalf("elector did not become primary")
	}
	if e.Primary() != "pod-a" {
		t.Errorf("Primary() = %q, want pod-a", e.Primary())
	}
	if _, lastErr := e.Health(); lastErr != "" {
		t.Errorf("lastErr = %q, want empty", lastErr)
	}
}

func TestElectorFollowerReportsTheRealHolder(t *testing.T) {
	api := &fakeAPIServer{}
	srv := httptest.NewServer(api.handler())
	defer srv.Close()

	leader := testElector(t, srv, "pod-a")
	leader.step(context.Background())

	follower := testElector(t, srv, "pod-b")
	follower.step(context.Background())

	if follower.IsPrimary() {
		t.Errorf("second elector also became primary")
	}
	if got := follower.Primary(); got != "pod-a" {
		t.Errorf("follower Primary() = %q, want pod-a", got)
	}
}

// Continuing to act as primary while unable to renew is how two pods end up
// pulling at once, so the elector must step down once its lease could have
// expired elsewhere.
func TestElectorStepsDownWhenItCannotRenew(t *testing.T) {
	api := &fakeAPIServer{}
	srv := httptest.NewServer(api.handler())

	e := testElector(t, srv, "pod-a")
	base := time.Now()
	e.now = func() time.Time { return base }
	e.step(context.Background())
	if !e.IsPrimary() {
		t.Fatalf("elector did not become primary")
	}

	srv.Close()

	// Still inside the lease duration: hold on, a blip is not a handover.
	e.now = func() time.Time { return base.Add(e.ttl / 2) }
	e.step(context.Background())
	if !e.IsPrimary() {
		t.Errorf("stepped down while the lease was still valid")
	}

	// Past it: the lease may already belong to someone else.
	e.now = func() time.Time { return base.Add(2 * e.ttl) }
	e.step(context.Background())
	if e.IsPrimary() {
		t.Errorf("still primary after failing to renew past the lease duration")
	}
	if _, lastErr := e.Health(); lastErr == "" {
		t.Errorf("lastErr is empty after repeated failures")
	}
}

func testElector(t *testing.T, srv *httptest.Server, identity string) *LeaseElector {
	t.Helper()

	cfg := testConfig(t)
	cfg.PodName = identity
	e := NewLeaseElector(newTestLeaseClient(t, srv), cfg, discardLogger())
	return e
}

func TestLeaseClientRequiresInClusterEnv(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	if _, err := NewLeaseClient(testConfig(t)); err == nil {
		t.Fatalf("NewLeaseClient succeeded outside a cluster")
	} else if !strings.Contains(err.Error(), "not running in a cluster") {
		t.Errorf("err = %v, want an out-of-cluster explanation", err)
	}
}
