package main

import (
	"sync"
	"sync/atomic"
	"time"
)

// AppState records what happened on the last pull for one application, so
// /status and /monitor can explain a stall without access to pod logs.
type AppState struct {
	mu                  sync.RWMutex
	lastPullAt          time.Time
	lastPullError       string
	consecutiveFailures int
}

func (s *AppState) recordSuccess(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastPullAt = at
	s.lastPullError = ""
	s.consecutiveFailures = 0
}

func (s *AppState) recordFailure(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastPullError = err.Error()
	s.consecutiveFailures++
}

func (s *AppState) snapshot() (lastPullAt time.Time, lastErr string, failures int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastPullAt, s.lastPullError, s.consecutiveFailures
}

// State is the process-wide view the health and introspection endpoints
// read. Nothing here gates serving: it explains behaviour, it does not
// control it.
type State struct {
	StartedAt time.Time

	apps map[string]*AppState

	// heartbeat is bumped by the sync loop. Liveness fails when it stops
	// advancing, which is the one condition a restart actually fixes.
	heartbeat atomic.Int64
}

func NewState(appIDs []string, now time.Time) *State {
	s := &State{StartedAt: now, apps: make(map[string]*AppState, len(appIDs))}
	for _, id := range appIDs {
		s.apps[id] = &AppState{}
	}
	s.beat(now)
	return s
}

func (s *State) App(appID string) *AppState {
	return s.apps[appID]
}

func (s *State) beat(now time.Time) {
	s.heartbeat.Store(now.UnixMilli())
}

func (s *State) LastBeat() time.Time {
	return time.UnixMilli(s.heartbeat.Load())
}
