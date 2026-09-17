package main

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Alarm names. They appear verbatim in the /monitor body, which is what the
// uptime check's alert text will quote.
const (
	alarmNoData          = "no_data"
	alarmNoPrimary       = "no_primary"
	alarmSnapshotStale   = "snapshot_stale"
	alarmReplicasDiverge = "replicas_diverged"
	alarmPeerUnhealthy   = "peer_unhealthy"
	alarmPullFailing     = "pull_failing"
)

// Alarm is one fleet-level condition. Firing distinguishes "true right now"
// from "true for long enough to be worth waking someone".
type Alarm struct {
	Name   string `json:"name"`
	Firing bool   `json:"firing"`
	Since  string `json:"since,omitempty"`
	Detail string `json:"detail"`
}

// ClusterAppView is one application's state across the whole fleet.
type ClusterAppView struct {
	App              string   `json:"app"`
	InSync           bool     `json:"in_sync"`
	Hashes           []string `json:"hashes"`
	NewestAgeSeconds int64    `json:"newest_age_seconds"`
	// PullAgeSeconds is how long ago any pod last reached upstream
	// successfully. It is -1 when no pod has ever managed one.
	PullAgeSeconds int64 `json:"pull_age_seconds"`
	Rows           int   `json:"rows"`
	ServablePods   int   `json:"servable_pods"`
}

// ClusterView is the whole fleet as one pod sees it.
type ClusterView struct {
	AnsweredBy  string           `json:"answered_by"`
	Primary     string           `json:"primary"`
	PodCount    int              `json:"pod_count"`
	ReadyCount  int              `json:"ready_count"`
	InSync      bool             `json:"in_sync"`
	Degraded    bool             `json:"degraded"`
	Pods        []PeerInfo       `json:"pods"`
	Unreachable []string         `json:"unreachable"`
	Apps        []ClusterAppView `json:"apps"`
	Alarms      []Alarm          `json:"alarms"`
}

// AlarmTracker turns instantaneous conditions into sustained ones. Without
// it, normal propagation lag between pods would page someone every import.
type AlarmTracker struct {
	mu    sync.Mutex
	since map[string]time.Time
}

func NewAlarmTracker() *AlarmTracker {
	return &AlarmTracker{since: make(map[string]time.Time)}
}

// condition is a raw observation, before any duration threshold applies.
type condition struct {
	name   string
	active bool
	detail string
	// after is how long the condition must hold before it fires. Zero fires
	// immediately.
	after time.Duration
}

func (t *AlarmTracker) evaluate(now time.Time, conds []condition) []Alarm {
	t.mu.Lock()
	defer t.mu.Unlock()

	out := make([]Alarm, 0, len(conds))
	seen := make(map[string]struct{}, len(conds))

	for _, c := range conds {
		seen[c.name] = struct{}{}
		if !c.active {
			delete(t.since, c.name)
			continue
		}

		start, ok := t.since[c.name]
		if !ok {
			start = now
			t.since[c.name] = start
		}

		alarm := Alarm{Name: c.name, Detail: c.detail, Since: start.UTC().Format(time.RFC3339)}
		alarm.Firing = now.Sub(start) >= c.after
		out = append(out, alarm)
	}

	for name := range t.since {
		if _, ok := seen[name]; !ok {
			delete(t.since, name)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// clusterView returns the fleet view, cached for CLUSTER_CACHE_TTL so a
// public request cannot amplify into one internal request per pod on every
// hit.
func (s *Server) clusterView(ctx context.Context) ClusterView {
	s.clusterMu.Lock()
	defer s.clusterMu.Unlock()

	now := s.now()
	if !s.clusterAt.IsZero() && now.Sub(s.clusterAt) < s.cfg.ClusterCacheTTL {
		return s.clusterCache
	}

	// Detached from the caller: the view is cached and feeds the alarm
	// timers, so a client that disconnects mid-survey would otherwise
	// publish a one-pod fleet to everyone else and reset the timers of
	// alarms that were legitimately accumulating.
	buildCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.PeerTimeout+2*time.Second)
	defer cancel()

	view := s.buildClusterView(buildCtx, now)
	s.clusterCache, s.clusterAt = view, now
	return view
}

func (s *Server) buildClusterView(ctx context.Context, now time.Time) ClusterView {
	self := s.peerInfo()
	self.Addr = s.cfg.PodIP

	pods := []PeerInfo{self}
	var unreachable []string
	if s.peers != nil {
		peerInfos, unreached := s.peers.Survey(ctx)
		pods = append(pods, peerInfos...)
		unreachable = unreached
	}
	sort.Slice(pods, func(i, j int) bool { return pods[i].Pod < pods[j].Pod })

	view := ClusterView{
		AnsweredBy:  s.cfg.PodName,
		Primary:     primaryOf(pods),
		PodCount:    len(pods) + len(unreachable),
		Pods:        pods,
		Unreachable: unreachable,
		InSync:      true,
	}
	for _, p := range pods {
		if p.Ready {
			view.ReadyCount++
		}
	}

	for _, id := range s.registry.IDs() {
		app := appViewAcross(pods, id, now)
		if !app.InSync {
			view.InSync = false
		}
		view.Apps = append(view.Apps, app)
	}

	view.Alarms = s.alarms.evaluate(now, s.conditions(view, now))
	for _, a := range view.Alarms {
		if a.Firing {
			view.Degraded = true
		}
	}
	return view
}

func primaryOf(pods []PeerInfo) string {
	for _, p := range pods {
		if p.Primary {
			return p.Pod
		}
	}
	return ""
}

// appViewAcross summarises one application over every reachable pod. Pods
// without the snapshot yet are not counted as divergence: a pod that has
// never loaded anything is a readiness problem, not a consistency one.
func appViewAcross(pods []PeerInfo, appID string, now time.Time) ClusterAppView {
	view := ClusterAppView{App: appID, InSync: true, PullAgeSeconds: -1}

	seen := make(map[string]struct{})
	var newest, lastPull time.Time

	for _, p := range pods {
		// Only the primary pulls, so the freshest pull time across the
		// fleet is the one that describes the fleet.
		if at, err := time.Parse(time.RFC3339, p.appLastPull(appID)); err == nil && at.After(lastPull) {
			lastPull = at
		}

		app, ok := p.app(appID)
		if !ok || !app.Servable || app.Hash == "" {
			continue
		}
		view.ServablePods++
		view.Rows = max(view.Rows, app.Rows)
		seen[app.Hash] = struct{}{}

		if at, err := time.Parse(time.RFC3339, app.ImportedAt); err == nil && at.After(newest) {
			newest = at
		}
	}

	if !lastPull.IsZero() {
		view.PullAgeSeconds = int64(now.Sub(lastPull).Seconds())
	}

	for h := range seen {
		view.Hashes = append(view.Hashes, h)
	}
	sort.Strings(view.Hashes)
	view.InSync = len(seen) <= 1

	if !newest.IsZero() {
		view.NewestAgeSeconds = int64(now.Sub(newest).Seconds())
	}
	return view
}

// conditions maps the fleet view onto the alarm set. Only /monitor acts on
// these; nothing here can make a pod unready.
func (s *Server) conditions(view ClusterView, now time.Time) []condition {
	conds := make([]condition, 0, 6)

	noData := view.ReadyCount == 0
	conds = append(conds, condition{
		name:   alarmNoData,
		active: noData,
		detail: "no pod holds servable translations",
	})

	// With election off there is no lease to hold, so the alarm is moot.
	if s.cfg.LeaderElection == electionLease {
		detail := "no pod holds the lease; nothing is pulling from upstream"
		if h, ok := s.elector.(electorHealth); ok {
			if _, lastErr := h.Health(); lastErr != "" {
				detail += ": " + lastErr
			}
		}
		conds = append(conds, condition{
			name:   alarmNoPrimary,
			active: view.Primary == "",
			after:  s.cfg.AlarmNoPrimary,
			detail: detail,
		})
	}

	unhealthy := len(view.Unreachable)
	for _, p := range view.Pods {
		if !p.Ready {
			unhealthy++
		}
	}
	conds = append(conds, condition{
		name:   alarmPeerUnhealthy,
		active: unhealthy > 0,
		after:  s.cfg.AlarmPeerUnready,
		detail: fmt.Sprintf("%d of %d pods are unready or unreachable", unhealthy, view.PodCount),
	})

	for _, app := range view.Apps {
		// Measured from the last successful pull, not from the snapshot's
		// import time: an export that simply does not change leaves
		// ImportedAt pinned, and measuring that would alert on a healthy
		// fleet serving stable content. A fleet that has never pulled is
		// covered by no_primary and pull_failing instead.
		stale := app.PullAgeSeconds >= 0 && time.Duration(app.PullAgeSeconds)*time.Second > s.cfg.AlarmSnapshotAge
		conds = append(conds, condition{
			name:   alarmSnapshotStale + ":" + app.App,
			active: stale,
			detail: fmt.Sprintf("%s: last successful pull was %ds ago", app.App, app.PullAgeSeconds),
		})

		conds = append(conds, condition{
			name:   alarmReplicasDiverge + ":" + app.App,
			active: !app.InSync,
			after:  s.cfg.AlarmDivergence,
			detail: fmt.Sprintf("%s: pods hold %d different snapshots: %v", app.App, len(app.Hashes), app.Hashes),
		})

		failing, worst := worstPullFailure(view.Pods, app.App)
		conds = append(conds, condition{
			name:   alarmPullFailing + ":" + app.App,
			active: failing >= s.cfg.AlarmPullFailures,
			detail: fmt.Sprintf("%s: %d consecutive pull failures: %s", app.App, failing, worst),
		})
	}
	return conds
}

// worstPullFailure reports the failure streak of the pod that is actually
// pulling. Only the primary runs pullApp, and only pullApp clears the count,
// so a pod demoted mid-outage carries its streak for the rest of its life.
// Counting every pod would keep this alarm firing long after a new primary
// recovered.
func worstPullFailure(pods []PeerInfo, appID string) (int, string) {
	worst, detail := 0, ""
	for _, p := range pods {
		if !p.Primary {
			continue
		}
		app, ok := p.app(appID)
		if !ok || app.Failures <= worst {
			continue
		}
		worst, detail = app.Failures, app.LastPullError
	}
	return worst, detail
}
