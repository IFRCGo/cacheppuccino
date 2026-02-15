package main

import "sync/atomic"

type ReadyState struct {
	ready atomic.Bool
}

func (s *ReadyState) SetReady(v bool) {
	s.ready.Store(v)
}

func (s *ReadyState) IsReady() bool {
	return s.ready.Load()
}
