package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Internal endpoints. They are served on a second listener that is not in
// the Service or the ingress: the public ingress routes "/" as a prefix, so
// anything on the public port is reachable from the internet.
const (
	internalPeerPath     = "/internal/peer"
	internalSnapshotPath = "/internal/snapshot"

	headerSnapshotHash       = "X-Snapshot-Hash"
	headerSnapshotImportedAt = "X-Snapshot-Imported-At"
	headerSnapshotRows       = "X-Snapshot-Rows"
)

// PeerAppInfo is one application's state as a peer reports it.
type PeerAppInfo struct {
	App        string `json:"app"`
	Hash       string `json:"hash"`
	ImportedAt string `json:"imported_at"`
	Rows       int    `json:"rows"`
	Servable   bool   `json:"servable"`
}

// Times on the wire are RFC3339Nano: snapshot versions are compared in
// milliseconds, and second-resolution timestamps would make two pods holding
// identical content disagree about its version.

// PeerInfo is a peer's self-description. It is deliberately small: every pod
// polls every other pod on a short interval, so this is the hot path.
type PeerInfo struct {
	Pod     string        `json:"pod"`
	Addr    string        `json:"addr,omitempty"`
	Version string        `json:"version"`
	Primary bool          `json:"primary"`
	Ready   bool          `json:"ready"`
	Apps    []PeerAppInfo `json:"apps"`
}

func (p PeerInfo) app(appID string) (PeerAppInfo, bool) {
	for _, a := range p.Apps {
		if a.App == appID {
			return a, true
		}
	}
	return PeerAppInfo{}, false
}

// PeerClient discovers sibling pods through the headless Service and pulls
// snapshots from them. Hydrating from a peer is how a new pod becomes ready
// without contacting upstream, which is the whole point: upstream may be
// down exactly when a pod is replaced.
type PeerClient struct {
	service string
	port    string
	selfPod string
	selfIP  string
	timeout time.Duration
	http    *http.Client
	logger  *slog.Logger

	// resolve is a seam: tests supply peers directly instead of standing up
	// DNS.
	resolve func(ctx context.Context, host string) ([]string, error)
}

func NewPeerClient(cfg Config, logger *slog.Logger) *PeerClient {
	_, port, err := net.SplitHostPort(cfg.InternalListenAddr)
	if err != nil {
		port = "8081"
	}
	return &PeerClient{
		service: cfg.PeerService,
		port:    port,
		selfPod: cfg.PodName,
		selfIP:  cfg.PodIP,
		timeout: cfg.PeerTimeout,
		http:    &http.Client{Timeout: cfg.PeerTimeout},
		logger:  logger,
		resolve: func(ctx context.Context, host string) ([]string, error) {
			return net.DefaultResolver.LookupHost(ctx, host)
		},
	}
}

// Discover lists sibling addresses. The headless Service publishes not-ready
// addresses too, so a broken pod stays visible -- otherwise it would vanish
// from /cluster in exactly the situation worth seeing.
func (p *PeerClient) Discover(ctx context.Context) []string {
	if p.service == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	addrs, err := p.resolve(ctx, p.service)
	if err != nil {
		p.logger.Debug("peer: discovery failed", slog.String("service", p.service), slog.String("err", err.Error()))
		return nil
	}

	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if a == p.selfIP {
			continue
		}
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// Info fetches one peer's self-description.
func (p *PeerClient) Info(ctx context.Context, addr string) (PeerInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.urlFor(addr, internalPeerPath), nil)
	if err != nil {
		return PeerInfo{}, err
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return PeerInfo{}, err
	}
	defer resp.Body.Close()

	if !isHTTPSuccess(resp.StatusCode) {
		return PeerInfo{}, fmt.Errorf("peer %s: %s", addr, resp.Status)
	}

	var info PeerInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info); err != nil {
		return PeerInfo{}, err
	}
	info.Addr = addr
	return info, nil
}

// Survey polls every peer concurrently. Unreachable peers are reported as
// such rather than dropped, so the caller can tell "no peers" from "a peer
// is down".
func (p *PeerClient) Survey(ctx context.Context) ([]PeerInfo, []string) {
	addrs := p.Discover(ctx)
	if len(addrs) == 0 {
		return nil, nil
	}

	var (
		mu          sync.Mutex
		infos       []PeerInfo
		unreachable []string
		wg          sync.WaitGroup
	)
	for _, addr := range addrs {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			info, err := p.Info(ctx, addr)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				unreachable = append(unreachable, addr)
				return
			}
			infos = append(infos, info)
		}(addr)
	}
	wg.Wait()

	sort.Slice(infos, func(i, j int) bool { return infos[i].Pod < infos[j].Pod })
	sort.Strings(unreachable)
	return infos, unreachable
}

// Hydrate fetches the newest snapshot any peer holds for appID. The primary
// is preferred, but any peer will do: during a failover the primary may be
// the pod that just went away, and a booting pod still needs somewhere to
// hydrate from.
func (p *PeerClient) Hydrate(ctx context.Context, appID string) (*Snapshot, []byte, bool) {
	infos, _ := p.Survey(ctx)

	best, bestApp, found := pickHydrationSource(infos, appID)
	if !found {
		return nil, nil, false
	}

	xlsx, hash, importedAt, err := p.fetchSnapshot(ctx, best.Addr, appID)
	if err != nil {
		p.logger.Debug("peer: snapshot fetch failed",
			slog.String("peer", best.Pod), slog.String("app", appID), slog.String("err", err.Error()))
		return nil, nil, false
	}
	if hash != bestApp.Hash {
		// The peer moved on between the survey and the fetch; next tick.
		return nil, nil, false
	}

	rows, err := ParseXLSX(xlsx)
	if err != nil {
		p.logger.Warn("peer: snapshot did not parse",
			slog.String("peer", best.Pod), slog.String("app", appID), slog.String("err", err.Error()))
		return nil, nil, false
	}

	// The origin's import time travels with the snapshot: stamping our own
	// would make versions drift between pods holding identical content.
	snap := NewSnapshot(appID, hash, importedAt, rows, xlsx)
	if !snap.Servable() {
		return nil, nil, false
	}
	return snap, xlsx, true
}

// pickHydrationSource chooses the peer holding the newest servable snapshot,
// breaking ties towards the primary.
func pickHydrationSource(infos []PeerInfo, appID string) (PeerInfo, PeerAppInfo, bool) {
	var (
		best     PeerInfo
		bestApp  PeerAppInfo
		bestTime time.Time
		found    bool
	)
	for _, info := range infos {
		app, ok := info.app(appID)
		if !ok || !app.Servable || app.Hash == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339, app.ImportedAt)
		if err != nil {
			continue
		}
		if found && !at.After(bestTime) && !(at.Equal(bestTime) && info.Primary) {
			continue
		}
		best, bestApp, bestTime, found = info, app, at, true
	}
	return best, bestApp, found
}

func (p *PeerClient) fetchSnapshot(ctx context.Context, addr, appID string) ([]byte, string, time.Time, error) {
	// Transferring the file rather than a parsed form keeps one parse path
	// and lets the receiver verify the bytes against the advertised hash.
	ctx, cancel := context.WithTimeout(ctx, p.timeout+30*time.Second)
	defer cancel()

	url := p.urlFor(addr, internalSnapshotPath) + "?app=" + appID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", time.Time{}, err
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, "", time.Time{}, err
	}
	defer resp.Body.Close()

	if !isHTTPSuccess(resp.StatusCode) {
		return nil, "", time.Time{}, fmt.Errorf("peer %s: %s", addr, resp.Status)
	}

	body, err := readAllLimited(resp.Body, maxXLSXBytes)
	if err != nil {
		return nil, "", time.Time{}, err
	}

	advertised := resp.Header.Get(headerSnapshotHash)
	if got := HashBytes(body); got != advertised {
		return nil, "", time.Time{}, fmt.Errorf("peer %s: hash mismatch: got %s, advertised %s", addr, got, advertised)
	}

	importedAt, err := time.Parse(time.RFC3339, resp.Header.Get(headerSnapshotImportedAt))
	if err != nil {
		return nil, "", time.Time{}, fmt.Errorf("peer %s: bad %s: %w", addr, headerSnapshotImportedAt, err)
	}
	return body, advertised, importedAt, nil
}

// urlFor builds a peer URL. Discovery normally yields bare IPs, which need
// the internal port appended; an address that already carries one is used as
// given.
func (p *PeerClient) urlFor(addr, path string) string {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, p.port)
	}
	return "http://" + addr + path
}
