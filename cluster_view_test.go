package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fleetPublic exposes one node's public routes, which is what an uptime
// check or an operator actually hits.
func fleetPublic(t *testing.T, n *testNode) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(n.server.routes())
	t.Cleanup(ts.Close)
	return ts
}

func getMonitor(t *testing.T, ts *httptest.Server) (int, MonitorResponse) {
	t.Helper()

	resp, err := http.Get(ts.URL + "/monitor")
	if err != nil {
		t.Fatalf("GET /monitor: %v", err)
	}
	defer resp.Body.Close()

	var env struct {
		Ok   bool            `json:"ok"`
		Data MonitorResponse `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode /monitor: %v", err)
	}
	return resp.StatusCode, env.Data
}

func getCluster(t *testing.T, ts *httptest.Server) ClusterView {
	t.Helper()

	resp, err := http.Get(ts.URL + "/cluster")
	if err != nil {
		t.Fatalf("GET /cluster: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /cluster = %d, want 200", resp.StatusCode)
	}

	var env struct {
		Ok   bool        `json:"ok"`
		Data ClusterView `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode /cluster: %v", err)
	}
	return env.Data
}

func TestClusterViewSeesWholeFleet(t *testing.T) {
	ctx := context.Background()
	nodes := newFleet(t, 3)

	for _, n := range nodes {
		n.src.body = seedXLSX(t)
	}
	nodes[0].syncer.pullApp(ctx, defaultAppID, "test")
	for _, n := range nodes[1:] {
		n.syncer.syncAppFromPeers(ctx, defaultAppID, 0)
	}

	view := getCluster(t, fleetPublic(t, nodes[0]))

	if view.PodCount != 3 {
		t.Errorf("pod_count = %d, want 3", view.PodCount)
	}
	if view.ReadyCount != 3 {
		t.Errorf("ready_count = %d, want 3", view.ReadyCount)
	}
	if !view.InSync {
		t.Errorf("in_sync = false, want true once the fleet has converged")
	}
	if view.AnsweredBy != nodes[0].cfg.PodName {
		t.Errorf("answered_by = %q, want %q", view.AnsweredBy, nodes[0].cfg.PodName)
	}
	if len(view.Apps) != 1 || len(view.Apps[0].Hashes) != 1 {
		t.Errorf("apps = %+v, want one app with one hash", view.Apps)
	}
}

// Divergence is the thing the fleet is designed to avoid, so it has to be
// visible rather than inferred.
func TestClusterViewReportsDivergence(t *testing.T) {
	ctx := context.Background()
	nodes := newFleet(t, 2)

	nodes[0].src.body = seedXLSX(t)
	nodes[0].syncer.pullApp(ctx, defaultAppID, "test")

	nodes[1].src.body = translationsXLSX(t, [][]string{
		{"page", "key", "en"},
		{"home", "greeting", "Different"},
	})
	nodes[1].syncer.pullApp(ctx, defaultAppID, "test")

	view := getCluster(t, fleetPublic(t, nodes[0]))

	if view.InSync {
		t.Errorf("in_sync = true while pods hold different snapshots")
	}
	if len(view.Apps[0].Hashes) != 2 {
		t.Errorf("hashes = %v, want two distinct", view.Apps[0].Hashes)
	}
}

func TestMonitorHealthyFleetIsOK(t *testing.T) {
	ctx := context.Background()
	nodes := newFleet(t, 2)

	for _, n := range nodes {
		n.src.body = seedXLSX(t)
	}
	nodes[0].syncer.pullApp(ctx, defaultAppID, "test")
	nodes[1].syncer.syncAppFromPeers(ctx, defaultAppID, 0)

	code, body := getMonitor(t, fleetPublic(t, nodes[0]))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (alarms: %+v)", code, body.Alarms)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want ok", body.Status)
	}
}

func TestMonitorGoesRedWithoutData(t *testing.T) {
	nodes := newFleet(t, 2)

	code, body := getMonitor(t, fleetPublic(t, nodes[0]))
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", code)
	}
	if !hasAlarm(body.Alarms, alarmNoData) {
		t.Errorf("alarms = %+v, want %s", body.Alarms, alarmNoData)
	}
}

// The alert exists so staleness is noticed, since nothing falls back
// automatically when no pod can pull.
func TestMonitorGoesRedOnStaleSnapshot(t *testing.T) {
	ctx := context.Background()
	nodes := newFleet(t, 1)
	node := nodes[0]

	node.src.body = seedXLSX(t)
	node.syncer.pullApp(ctx, defaultAppID, "test")

	node.server.now = func() time.Time {
		return time.Now().Add(2 * node.cfg.AlarmSnapshotAge)
	}

	code, body := getMonitor(t, fleetPublic(t, node))
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", code)
	}
	if !hasAlarm(body.Alarms, alarmSnapshotStale+":"+defaultAppID) {
		t.Errorf("alarms = %+v, want a stale-snapshot alarm", body.Alarms)
	}
}

// A degraded fleet must never make a pod unready: taking pods out of service
// because their data is old turns a freshness problem into an outage.
func TestMonitorRedDoesNotAffectReadiness(t *testing.T) {
	ctx := context.Background()
	nodes := newFleet(t, 1)
	node := nodes[0]

	node.src.body = seedXLSX(t)
	node.syncer.pullApp(ctx, defaultAppID, "test")

	// The snapshot ages while the sync loop keeps ticking: upstream is gone,
	// the process is fine.
	stale := time.Now().Add(2 * node.cfg.AlarmSnapshotAge)
	node.server.now = func() time.Time { return stale }
	node.state.beat(stale)

	ts := fleetPublic(t, node)
	if code, _ := getMonitor(t, ts); code != http.StatusServiceUnavailable {
		t.Fatalf("/monitor = %d, want 503", code)
	}

	for _, path := range []string{"/readyz", "/healthz", "/strings?lang=en&page=home"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 while only /monitor is red", path, resp.StatusCode)
		}
	}
}

// Brief divergence during propagation is normal; only a sustained one is
// worth an alert.
func TestDivergenceAlarmNeedsToBeSustained(t *testing.T) {
	tracker := NewAlarmTracker()
	base := time.Now()

	conds := []condition{{name: alarmReplicasDiverge, active: true, after: 2 * time.Minute, detail: "two hashes"}}

	alarms := tracker.evaluate(base, conds)
	if len(alarms) != 1 || alarms[0].Firing {
		t.Fatalf("alarm fired immediately: %+v", alarms)
	}

	alarms = tracker.evaluate(base.Add(time.Minute), conds)
	if alarms[0].Firing {
		t.Errorf("alarm fired before its threshold")
	}

	alarms = tracker.evaluate(base.Add(3*time.Minute), conds)
	if !alarms[0].Firing {
		t.Errorf("alarm did not fire after its threshold")
	}
}

// A condition that clears must reset, so a later recurrence starts its own
// clock rather than firing instantly.
func TestAlarmClearsAndRestartsItsClock(t *testing.T) {
	tracker := NewAlarmTracker()
	base := time.Now()

	firing := []condition{{name: alarmNoPrimary, active: true, after: time.Minute}}
	clear := []condition{{name: alarmNoPrimary, active: false, after: time.Minute}}

	tracker.evaluate(base, firing)
	if alarms := tracker.evaluate(base.Add(2*time.Minute), firing); !alarms[0].Firing {
		t.Fatalf("alarm did not fire after its threshold")
	}

	if alarms := tracker.evaluate(base.Add(3*time.Minute), clear); len(alarms) != 0 {
		t.Fatalf("cleared condition still reported: %+v", alarms)
	}

	alarms := tracker.evaluate(base.Add(4*time.Minute), firing)
	if len(alarms) != 1 || alarms[0].Firing {
		t.Errorf("recurrence fired immediately instead of restarting its clock: %+v", alarms)
	}
}

func TestClusterViewIsCached(t *testing.T) {
	ctx := context.Background()
	nodes := newFleet(t, 2)
	nodes[0].src.body = seedXLSX(t)
	nodes[0].syncer.pullApp(ctx, defaultAppID, "test")

	// A public endpoint that fanned out on every hit would let anyone
	// amplify one request into one per pod.
	before := requestsTo(t, nodes[1])
	for range 5 {
		getCluster(t, fleetPublic(t, nodes[0]))
	}
	after := requestsTo(t, nodes[1])

	if after-before > 1 {
		t.Errorf("peer received %d requests for 5 /cluster hits, want at most 1", after-before)
	}
}

func hasAlarm(alarms []Alarm, name string) bool {
	for _, a := range alarms {
		if a.Name == name {
			return true
		}
	}
	return false
}
