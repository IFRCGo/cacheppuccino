package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// testNode is one complete server instance: its own registry, syncer, stub
// upstream, and a real internal listener the other nodes talk to.
type testNode struct {
	cfg      Config
	registry *Registry
	state    *State
	syncer   *Syncer
	server   *Server
	peers    *PeerClient
	elector  Elector
	src      *stubSource
	internal *httptest.Server
	addr     string

	// requests counts what other pods asked of this one, so tests can prove
	// the cluster view is cached rather than fanning out per hit.
	requests atomic.Int64

	// transfers counts snapshot downloads specifically, which is the
	// expensive half of hydration.
	transfers atomic.Int64
}

func (n *testNode) snapshot(t *testing.T) *Snapshot {
	t.Helper()
	h, ok := n.registry.Holder(defaultAppID)
	if !ok {
		t.Fatalf("no holder for %q", defaultAppID)
	}
	return h.Load()
}

func (n *testNode) hash(t *testing.T) string {
	t.Helper()
	if s := n.snapshot(t); s != nil {
		return s.Hash
	}
	return ""
}

// newFleet stands up n complete nodes wired to each other over real HTTP.
// Only DNS is faked: everything else -- handlers, hash verification, XLSX
// parsing, version comparison -- is the production path.
func newFleet(t *testing.T, n int) []*testNode {
	t.Helper()

	nodes := make([]*testNode, 0, n)
	for i := range n {
		node := newFleetNode(t, i)
		nodes = append(nodes, node)
	}

	addrs := make([]string, 0, n)
	for _, node := range nodes {
		addrs = append(addrs, node.addr)
	}

	for _, node := range nodes {
		self := node.addr
		node.peers.resolve = func(context.Context, string) ([]string, error) {
			return addrs, nil
		}
		node.peers.selfIP = self
	}
	return nodes
}

func newFleetNode(t *testing.T, i int) *testNode {
	t.Helper()

	cfg := testConfig(t)
	cfg.PodName = "pod-" + string(rune('a'+i))
	cfg.PeerService = "cacheppuccino-headless"
	cfg.CacheDir = t.TempDir()

	registry := NewRegistry(cfg.AppIDs())
	state := NewState(cfg.AppIDs(), time.Now())
	cache := NewCache(cfg.CacheDir, cfg.MaxCacheAge)
	elector := Elector(alwaysPrimary{identity: cfg.PodName})

	src := &stubSource{}
	syncer := NewSyncer(cfg, registry, cache, state, elector, discardLogger())
	syncer.sources[defaultAppID] = src

	server := NewServer(cfg, registry, state, syncer, elector, discardLogger())
	peers := NewPeerClient(cfg, discardLogger())
	syncer.SetHydrator(peers)
	server.peers = peers

	// Fully populated before the listener accepts anything: assigning over
	// the struct afterwards would reset the counters the tests assert on,
	// and go vet's copylocks does not catch a composite literal.
	node := &testNode{
		cfg:      cfg,
		elector:  elector,
		registry: registry,
		state:    state,
		syncer:   syncer,
		server:   server,
		peers:    peers,
		src:      src,
	}
	counted := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		node.requests.Add(1)
		if r.URL.Path == internalSnapshotPath {
			node.transfers.Add(1)
		}
		server.internalRoutes().ServeHTTP(w, r)
	})
	internal := httptest.NewServer(counted)
	t.Cleanup(internal.Close)

	node.internal = internal
	node.addr = internal.Listener.Addr().String()
	return node
}

// A pod that has never run must become servable without reaching upstream:
// upstream is often exactly what is broken when a pod is replaced.
func TestColdNodeHydratesFromWarmPeer(t *testing.T) {
	ctx := context.Background()
	nodes := newFleet(t, 3)
	warm, cold := nodes[0], nodes[2]

	warm.src.body = seedXLSX(t)
	warm.syncer.pullApp(ctx, defaultAppID, "test")
	wantHash := warm.hash(t)

	if cold.snapshot(t) != nil {
		t.Fatalf("cold node started with data")
	}

	cold.syncer.syncAppFromPeers(ctx, defaultAppID, 0)

	if got := cold.hash(t); got != wantHash {
		t.Fatalf("cold node hash = %q, want %q", got, wantHash)
	}
	if cold.src.Calls() != 0 {
		t.Errorf("cold node made %d upstream calls, want 0", cold.src.Calls())
	}
	if !cold.registry.Servable() {
		t.Errorf("cold node is not servable after hydrating")
	}
}

// The import time travels with the snapshot, so pods holding identical
// content agree on its version instead of each stamping their own.
func TestHydrationPreservesOriginImportTime(t *testing.T) {
	ctx := context.Background()
	nodes := newFleet(t, 2)
	warm, cold := nodes[0], nodes[1]

	warm.src.body = seedXLSX(t)
	warm.syncer.pullApp(ctx, defaultAppID, "test")

	cold.syncer.syncAppFromPeers(ctx, defaultAppID, 0)

	if got, want := cold.snapshot(t).Version(), warm.snapshot(t).Version(); got != want {
		t.Errorf("version = %d, want %d", got, want)
	}
}

func TestFleetConvergesOnNewContent(t *testing.T) {
	ctx := context.Background()
	nodes := newFleet(t, 3)

	nodes[0].src.body = seedXLSX(t)
	nodes[0].syncer.pullApp(ctx, defaultAppID, "test")
	for _, n := range nodes[1:] {
		n.syncer.syncAppFromPeers(ctx, defaultAppID, 0)
	}

	// A later import on one node must reach the rest.
	nodes[0].syncer.now = func() time.Time { return time.Now().Add(time.Minute) }
	nodes[0].src.body = translationsXLSX(t, [][]string{
		{"page", "key", "en"},
		{"home", "greeting", "Hi again"},
	})
	nodes[0].syncer.pullApp(ctx, defaultAppID, "test")

	want := nodes[0].hash(t)
	for _, n := range nodes[1:] {
		n.syncer.syncAppFromPeers(ctx, defaultAppID, 0)
		if got := n.hash(t); got != want {
			t.Errorf("%s hash = %q, want %q", n.cfg.PodName, got, want)
		}
	}
}

// A lagging peer must never be able to walk a pod backwards to content it
// has already stopped serving.
func TestNodeDoesNotAdoptOlderPeerSnapshot(t *testing.T) {
	ctx := context.Background()
	nodes := newFleet(t, 2)
	lagging, ahead := nodes[0], nodes[1]

	lagging.src.body = seedXLSX(t)
	lagging.syncer.pullApp(ctx, defaultAppID, "test")

	ahead.syncer.now = func() time.Time { return time.Now().Add(time.Hour) }
	ahead.src.body = translationsXLSX(t, [][]string{
		{"page", "key", "en"},
		{"home", "greeting", "Newer"},
	})
	ahead.syncer.pullApp(ctx, defaultAppID, "test")
	newest := ahead.hash(t)

	// Forward: the lagging node takes the newer snapshot.
	lagging.syncer.syncAppFromPeers(ctx, defaultAppID, 0)
	if got := lagging.hash(t); got != newest {
		t.Fatalf("lagging node hash = %q, want %q", got, newest)
	}

	// Backward: the node that is ahead must ignore the older offer.
	ahead.syncer.syncAppFromPeers(ctx, defaultAppID, 0)
	if got := ahead.hash(t); got != newest {
		t.Errorf("node moved backwards to %q, want %q", got, newest)
	}
}

// Bytes are verified against the advertised hash, so a peer serving content
// that does not match what it claims is rejected rather than adopted.
func TestHydrationRejectsHashMismatch(t *testing.T) {
	ctx := context.Background()
	nodes := newFleet(t, 2)
	warm, cold := nodes[0], nodes[1]

	warm.src.body = seedXLSX(t)
	warm.syncer.pullApp(ctx, defaultAppID, "test")

	// Rewrite the served bytes while leaving the advertised hash alone.
	h, _ := warm.registry.Holder(defaultAppID)
	good := h.Load()
	h.Store(&Snapshot{
		AppID:      good.AppID,
		Hash:       good.Hash,
		ImportedAt: good.ImportedAt,
		RowCount:   good.RowCount,
		byPage:     good.byPage,
		probePage:  good.probePage,
		probeLang:  good.probeLang,
		raw:        []byte("tampered"),
	})

	cold.syncer.syncAppFromPeers(ctx, defaultAppID, 0)

	if cold.snapshot(t) != nil {
		t.Errorf("adopted a snapshot whose bytes did not match the advertised hash")
	}
}

func TestSurveyReportsUnreachablePeers(t *testing.T) {
	nodes := newFleet(t, 3)
	nodes[1].internal.Close()

	infos, unreachable := nodes[0].peers.Survey(context.Background())

	if len(infos) != 1 {
		t.Errorf("reachable peers = %d, want 1", len(infos))
	}
	// A pod that stops answering must show up as unreachable rather than
	// silently shrink the fleet size.
	if len(unreachable) != 1 || unreachable[0] != nodes[1].addr {
		t.Errorf("unreachable = %v, want [%s]", unreachable, nodes[1].addr)
	}
}

// electedFleet is newFleet with real Lease-based election against one shared
// fake API server, so exactly one node is primary at a time.
func newElectedFleet(t *testing.T, n int) ([]*testNode, *fakeAPIServer, *httptest.Server) {
	t.Helper()

	api := &fakeAPIServer{}
	apiSrv := httptest.NewServer(api.handler())
	t.Cleanup(apiSrv.Close)

	nodes := newFleet(t, n)
	for _, node := range nodes {
		e := NewLeaseElector(newTestLeaseClient(t, apiSrv), node.cfg, discardLogger())
		node.elector = e
		node.syncer.elector = e
		node.server.elector = e
	}
	return nodes, api, apiSrv
}

// Only the primary may talk to upstream; followers take their data from
// peers. A fleet where every pod pulled would multiply load on the
// translation API by the replica count.
func TestOnlyPrimaryPullsUpstream(t *testing.T) {
	ctx := context.Background()
	nodes, _, _ := newElectedFleet(t, 3)

	for _, n := range nodes {
		n.src.body = seedXLSX(t)
		n.elector.(*LeaseElector).step(ctx)
	}

	primaries := 0
	for _, n := range nodes {
		if n.elector.IsPrimary() {
			primaries++
		}
	}
	if primaries != 1 {
		t.Fatalf("primaries = %d, want 1", primaries)
	}

	for _, n := range nodes {
		n.syncer.pullAll(ctx, "test")
	}

	for _, n := range nodes {
		calls := n.src.Calls()
		if n.elector.IsPrimary() && calls != 1 {
			t.Errorf("primary %s made %d upstream calls, want 1", n.cfg.PodName, calls)
		}
		if !n.elector.IsPrimary() && calls != 0 {
			t.Errorf("follower %s made %d upstream calls, want 0", n.cfg.PodName, calls)
		}
	}
}

// When the primary stops renewing, another node must take over and start
// pulling, while every node keeps serving throughout.
func TestFleetPromotesNewPrimaryAfterFailure(t *testing.T) {
	ctx := context.Background()
	nodes, _, _ := newElectedFleet(t, 3)
	base := time.Now()

	for _, n := range nodes {
		n.src.body = seedXLSX(t)
		e := n.elector.(*LeaseElector)
		e.now = func() time.Time { return base }
		e.step(ctx)
	}

	var old *testNode
	for _, n := range nodes {
		if n.elector.IsPrimary() {
			old = n
		}
	}
	if old == nil {
		t.Fatalf("no primary elected")
	}
	old.syncer.pullAll(ctx, "test")

	// Everyone has data, so nothing about the handover affects serving.
	for _, n := range nodes[1:] {
		n.syncer.syncAppFromPeers(ctx, defaultAppID, 0)
	}

	// The primary stops renewing; the others try again past the lease TTL.
	after := base.Add(2 * old.cfg.LeaseDuration)
	for _, n := range nodes {
		if n == old {
			continue
		}
		e := n.elector.(*LeaseElector)
		e.now = func() time.Time { return after }
		e.step(ctx)
	}

	promoted := 0
	for _, n := range nodes {
		if n == old {
			continue
		}
		if n.elector.IsPrimary() {
			promoted++
			n.syncer.pullAll(ctx, "test")
			if n.src.Calls() == 0 {
				t.Errorf("promoted node %s did not pull upstream", n.cfg.PodName)
			}
		}
		if !n.registry.Servable() {
			t.Errorf("%s stopped being servable during a handover", n.cfg.PodName)
		}
	}
	if promoted != 1 {
		t.Errorf("promoted = %d, want exactly 1 successor", promoted)
	}
}

// requestsTo reports how many internal requests a node has served.
func requestsTo(t *testing.T, n *testNode) int64 {
	t.Helper()
	return n.requests.Load()
}

// A converged fleet must not keep transferring and re-parsing the export it
// already holds: the peer loop runs every PEER_POLL_INTERVAL forever, so a
// no-op poll that still downloads the file is permanent fleet-wide load.
func TestPeerSyncSkipsTransferWhenAlreadyCurrent(t *testing.T) {
	ctx := context.Background()
	nodes := newFleet(t, 3)
	warm, cold := nodes[0], nodes[2]

	warm.src.body = seedXLSX(t)
	warm.syncer.pullApp(ctx, defaultAppID, "test")

	cold.syncer.syncAppFromPeers(ctx, defaultAppID, 0)
	if cold.hash(t) != warm.hash(t) {
		t.Fatalf("cold node did not hydrate")
	}

	before := warm.transfers.Load()
	if before == 0 {
		t.Fatalf("hydration made no snapshot transfer")
	}
	for range 5 {
		cold.syncer.syncAppFromPeers(ctx, defaultAppID, 0)
	}
	if got := warm.transfers.Load(); got != before {
		t.Errorf("snapshot transfers = %d after 5 no-op polls, want %d", got, before)
	}
}
