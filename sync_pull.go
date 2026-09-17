package main

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Elector reports whether this pod is the primary, the one pod allowed to
// pull from upstream. Followers take their data from peers instead.
type Elector interface {
	IsPrimary() bool
	// Primary names the current holder, empty when nobody holds it.
	Primary() string
	Identity() string
}

// electorHealth is implemented by electors that talk to something that can
// fail. The no_primary alarm quotes the error when the elector has one.
type electorHealth interface {
	Health() (lastSuccess time.Time, lastErr string)
}

// alwaysPrimary is the elector used when leader election is off: a single
// container has nobody to coordinate with.
type alwaysPrimary struct{ identity string }

func (a alwaysPrimary) IsPrimary() bool  { return true }
func (a alwaysPrimary) Primary() string  { return a.identity }
func (a alwaysPrimary) Identity() string { return a.identity }

// Hydrator supplies a snapshot from somewhere other than upstream. Peer
// hydration implements it; a nil Hydrator means upstream is the only source.
// current is what this pod already holds, so an implementation can decline
// before paying for a transfer.
type Hydrator interface {
	Hydrate(ctx context.Context, appID string, current *Snapshot) (*Snapshot, []byte, bool)
}

// Syncer owns every path by which snapshots enter this process: the local
// cache at boot, peers, and upstream pulls.
type Syncer struct {
	cfg      Config
	registry *Registry
	cache    *Cache
	state    *State
	elector  Elector
	hydrator Hydrator
	sources  map[string]XLSXSource
	apps     map[string]AppConfig
	logger   *slog.Logger

	// now is a seam: the sync loops are timing-driven and tests must not
	// depend on wall-clock time.
	now func() time.Time

	wg sync.WaitGroup
}

func NewSyncer(cfg Config, registry *Registry, cache *Cache, state *State, elector Elector, logger *slog.Logger) *Syncer {
	sources := make(map[string]XLSXSource, len(cfg.Apps))
	apps := make(map[string]AppConfig, len(cfg.Apps))
	for _, a := range cfg.Apps {
		sources[a.ID] = NewSource(a, cfg.HTTPTimeout)
		apps[a.ID] = a
	}
	return &Syncer{
		cfg:      cfg,
		registry: registry,
		cache:    cache,
		state:    state,
		elector:  elector,
		sources:  sources,
		apps:     apps,
		logger:   logger,
		now:      time.Now,
	}
}

func (s *Syncer) SetHydrator(h Hydrator) { s.hydrator = h }

func (s *Syncer) SourceName(appID string) string {
	if src, ok := s.sources[appID]; ok {
		return src.Name()
	}
	return ""
}

// LoadFromCache seeds snapshots from local disk, so a restarted container
// serves immediately instead of waiting on a peer or on upstream.
func (s *Syncer) LoadFromCache() {
	now := s.now()
	for _, id := range s.registry.IDs() {
		xlsx, meta, ok := s.cache.Load(id, now)
		if !ok {
			continue
		}
		rows, err := ParseXLSX(xlsx)
		if err != nil {
			s.logger.Warn("cache: discarding unparseable entry", slog.String("app", id), slog.String("err", err.Error()))
			continue
		}
		snap := NewSnapshot(id, meta.Hash, meta.ImportedAt, rows, xlsx)
		if !snap.Servable() {
			continue
		}
		h, _ := s.registry.Holder(id)
		h.Store(snap)
		s.logger.Info("cache: loaded",
			slog.String("app", id),
			slog.String("hash", meta.Hash),
			slog.Int("rows", snap.RowCount),
			slog.Duration("age", now.Sub(meta.ImportedAt)),
		)
	}
}

// Run starts the sync loops and returns. The pull loop runs on the primary;
// the peer loop runs everywhere, including on the primary, so a primary that
// has just taken over can still hydrate from a peer that is ahead of it.
func (s *Syncer) Run(ctx context.Context) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.pullLoop(ctx)
	}()
	if s.hydrator != nil {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.peerLoop(ctx)
		}()
	}
}

// Wait blocks until the loops started by Run have returned, so a caller can
// shut down without killing a pull mid-write.
func (s *Syncer) Wait() { s.wg.Wait() }

// pullLoop pulls once at startup and then settles into the configured
// interval, retrying with a short backoff until the pod holds servable data.
// The startup pull runs even when the local cache already seeded a snapshot,
// so a restart does not serve cached content for a whole pull interval before
// checking upstream. The backoff matters: without it a pod that loses a
// transient upstream blip at startup stays unready for a whole pull interval,
// which stalls rollouts.
func (s *Syncer) pullLoop(ctx context.Context) {
	backoff := s.cfg.InitialPullBackoffMin

	for {
		if ctx.Err() != nil {
			return
		}
		s.state.beat(s.now())

		pullCtx, cancel := s.withDeadline(ctx, s.cfg.InitialPullDeadline)
		s.pullAll(pullCtx, "initial")
		cancel()
		if s.registry.Servable() {
			break
		}
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, s.cfg.InitialPullBackoffMax)
	}

	ticker := time.NewTicker(s.cfg.PullInterval)
	defer ticker.Stop()

	jitterMax := time.Duration(float64(s.cfg.PullInterval) * 0.10)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.state.beat(s.now())
			if jitterMax > 0 && !sleepCtx(ctx, jitter(s.now(), jitterMax)) {
				return
			}
			s.pullAll(ctx, "periodic")
		}
	}
}

// pullAll pulls every application concurrently, bounded by PULL_CONCURRENCY.
// One application's failure never blocks or fails another's.
//
// Only the primary talks to upstream; followers take their data from peers.
// The check lives here rather than at the call sites so no future caller can
// bypass it.
func (s *Syncer) pullAll(ctx context.Context, kind string) {
	if !s.elector.IsPrimary() {
		return
	}

	sem := make(chan struct{}, s.cfg.PullConcurrency)
	var wg sync.WaitGroup

	for _, id := range s.registry.IDs() {
		wg.Add(1)
		go func(appID string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			s.pullApp(ctx, appID, kind)
		}(id)
	}
	wg.Wait()
}

func (s *Syncer) pullApp(ctx context.Context, appID, kind string) {
	logger := s.logger.With(slog.String("app", appID))
	st := s.state.App(appID)

	src := s.sources[appID]
	fetchStart := s.now()

	xlsx, err := src.Fetch(ctx, logger)
	if err != nil {
		// Shutdown is not a pull failure; the parent context is the process one.
		if ctx.Err() == nil {
			st.recordFailure(err)
			s.logPullFailure(logger, appID, kind+" pull failed; serving cached data if any", err)
		}
		return
	}
	logger.Info("pull: download done",
		slog.Duration("dur", s.now().Sub(fetchStart)),
		slog.Int("bytes", len(xlsx)),
	)

	hash := HashBytes(xlsx)
	h, _ := s.registry.Holder(appID)

	if cur := h.Load(); cur != nil && cur.Hash == hash {
		st.recordSuccess(s.now())
		logger.Info(kind+" pull unchanged", slog.String("hash", hash))
		return
	}

	rows, err := ParseXLSX(xlsx)
	if err != nil {
		st.recordFailure(err)
		s.logPullFailure(logger, appID, kind+" pull parse failed; serving cached data if any", err)
		return
	}

	snap := NewSnapshot(appID, hash, s.now(), rows, xlsx)
	if !snap.Servable() {
		st.recordFailure(errEmptyImport)
		s.logPullFailure(logger, appID, kind+" pull produced no servable rows; keeping previous data", errEmptyImport)
		return
	}

	h.Store(snap)
	st.recordSuccess(s.now())
	s.saveToCache(appID, xlsx, snap)

	logger.Info(kind+" pull imported", slog.Int("rows", snap.RowCount), slog.String("hash", hash))
}

func (s *Syncer) withDeadline(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}

// sleepCtx waits for d, reporting false if the context ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func jitter(now time.Time, max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return time.Duration(now.UnixNano() % int64(max+1))
}

// peerLoop keeps this pod aligned with the rest of the fleet. It runs on
// every pod, including the primary: a pod that has just taken over the lease
// can still be behind a peer that adopted a newer snapshot first.
//
// Adoption is jittered because every pod learns of a new snapshot within one
// poll interval and then parses the same file. Without the spread, that
// parse happens fleet-wide at the same instant.
func (s *Syncer) peerLoop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.PeerPollInterval)
	defer ticker.Stop()

	adoptJitter := min(s.cfg.PeerPollInterval/2, 2*time.Second)

	// One pass before the first tick. A cold pod prefers a warm sibling over
	// upstream, and the pull loop starts its upstream attempt immediately,
	// so waiting out a poll interval here would invert that order.
	s.pollPeers(ctx, adoptJitter)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.state.beat(s.now())
			s.pollPeers(ctx, adoptJitter)
		}
	}
}

func (s *Syncer) pollPeers(ctx context.Context, adoptJitter time.Duration) {
	for _, id := range s.registry.IDs() {
		s.syncAppFromPeers(ctx, id, adoptJitter)
	}
}

func (s *Syncer) syncAppFromPeers(ctx context.Context, appID string, adoptJitter time.Duration) {
	h, _ := s.registry.Holder(appID)

	snap, xlsx, ok := s.hydrator.Hydrate(ctx, appID, h.Load())
	if !ok {
		return
	}
	if !sleepCtx(ctx, jitter(s.now(), adoptJitter)) {
		return
	}
	if !h.StoreIfNewer(snap) {
		return
	}

	s.saveToCache(appID, xlsx, snap)

	s.logger.Info("peer sync adopted",
		slog.String("app", appID),
		slog.String("hash", snap.Hash),
		slog.Int("rows", snap.RowCount),
	)
}

// saveToCache persists the bytes a snapshot was built from. A failed write
// costs a slow restart, not correctness.
func (s *Syncer) saveToCache(appID string, xlsx []byte, snap *Snapshot) {
	err := s.cache.Save(appID, xlsx, cacheMeta{
		Hash:       snap.Hash,
		ImportedAt: snap.ImportedAt,
		RowCount:   snap.RowCount,
	})
	if err != nil {
		s.logger.Warn("cache: write failed", slog.String("app", appID), slog.String("err", err.Error()))
	}
}

// logPullFailure escalates to error once the failures are sustained, so an
// alert keyed on error level fires on a real outage without firing on a
// single blip.
func (s *Syncer) logPullFailure(logger *slog.Logger, appID, msg string, err error) {
	_, _, failures := s.state.App(appID).snapshot()

	level := slog.LevelWarn
	if failures >= s.cfg.AlarmPullFailures {
		level = slog.LevelError
	}
	logger.Log(context.Background(), level, msg,
		slog.String("err", err.Error()),
		slog.Int("consecutive_failures", failures),
	)
}
