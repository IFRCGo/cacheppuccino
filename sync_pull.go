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

// alwaysPrimary is the elector used when leader election is off: a single
// container has nobody to coordinate with.
type alwaysPrimary struct{ identity string }

func (a alwaysPrimary) IsPrimary() bool  { return true }
func (a alwaysPrimary) Primary() string  { return a.identity }
func (a alwaysPrimary) Identity() string { return a.identity }

// Hydrator supplies a snapshot from somewhere other than upstream. Peer
// hydration implements it; a nil Hydrator means upstream is the only source.
type Hydrator interface {
	Hydrate(ctx context.Context, appID string) (*Snapshot, []byte, bool)
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

// LoadFromCache seeds snapshots from local disk before the server starts
// accepting traffic, so a restarted container serves immediately instead of
// waiting on a peer or on upstream.
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
		snap := NewSnapshot(id, meta.Hash, meta.ImportedAt, rows)
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
	go s.pullLoop(ctx)
	if s.hydrator != nil {
		go s.peerLoop(ctx)
	}
}

// pullLoop performs the initial hydration with a short backoff, then settles
// into the configured interval. The backoff matters: without it a pod that
// loses a transient upstream blip at startup stays unready for a whole pull
// interval, which stalls rollouts.
func (s *Syncer) pullLoop(ctx context.Context) {
	backoff := s.cfg.InitialPullBackoffMin

	for !s.registry.Servable() {
		if ctx.Err() != nil {
			return
		}
		s.state.beat(s.now())

		if s.elector.IsPrimary() {
			pullCtx, cancel := s.withDeadline(ctx, s.cfg.InitialPullDeadline)
			s.pullAll(pullCtx, "initial")
			cancel()
		}
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
			if s.elector.IsPrimary() {
				s.pullAll(ctx, "periodic")
			}
		}
	}
}

// pullAll pulls every application concurrently, bounded by PULL_CONCURRENCY.
// One application's failure never blocks or fails another's.
func (s *Syncer) pullAll(ctx context.Context, kind string) {
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
			logger.Warn(kind+" pull failed; serving cached data if any", slog.String("err", err.Error()))
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
		logger.Warn(kind+" pull parse failed; serving cached data if any", slog.String("err", err.Error()))
		return
	}

	snap := NewSnapshot(appID, hash, s.now(), rows)
	if !snap.Servable() {
		st.recordFailure(errEmptyImport)
		logger.Warn(kind + " pull produced no servable rows; keeping previous data")
		return
	}

	h.Store(snap)
	st.recordSuccess(s.now())

	if err := s.cache.Save(appID, xlsx, cacheMeta{
		Hash:       hash,
		ImportedAt: snap.ImportedAt,
		RowCount:   snap.RowCount,
	}); err != nil {
		// A failed cache write costs a slow restart, not correctness.
		logger.Warn("cache: write failed", slog.String("err", err.Error()))
	}

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

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.state.beat(s.now())
			for _, id := range s.registry.IDs() {
				s.syncAppFromPeers(ctx, id, adoptJitter)
			}
		}
	}
}

func (s *Syncer) syncAppFromPeers(ctx context.Context, appID string, adoptJitter time.Duration) {
	snap, xlsx, ok := s.hydrator.Hydrate(ctx, appID)
	if !ok {
		return
	}

	h, _ := s.registry.Holder(appID)
	if cur := h.Load(); cur != nil && (snap.Hash == cur.Hash || snap.Version() <= cur.Version()) {
		return
	}
	if !sleepCtx(ctx, jitter(s.now(), adoptJitter)) {
		return
	}
	if !h.StoreIfNewer(snap) {
		return
	}

	if err := s.cache.Save(appID, xlsx, cacheMeta{
		Hash:       snap.Hash,
		ImportedAt: snap.ImportedAt,
		RowCount:   snap.RowCount,
	}); err != nil {
		s.logger.Warn("cache: write failed", slog.String("app", appID), slog.String("err", err.Error()))
	}

	s.logger.Info("peer sync adopted",
		slog.String("app", appID),
		slog.String("hash", snap.Hash),
		slog.Int("rows", snap.RowCount),
	)
}
