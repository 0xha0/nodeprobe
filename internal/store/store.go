// Package store keeps the current nodes and the results of their tests, shared between the
// probe loops (writers) and the metrics and status handlers (readers, through Snapshot).
package store

import (
	"sort"
	"sync"
	"time"

	"github.com/0xha0/nodeprobe/internal/node"
	"github.com/0xha0/nodeprobe/internal/provider"
)

// Buckets are the latency histogram's upper bounds, in seconds.
var Buckets = []float64{0.05, 0.1, 0.15, 0.2, 0.3, 0.5, 0.75, 1, 2, 5}

// Speed results.
const (
	Pass = "pass"
	Slow = "slow"
	Fail = "fail"
)

// Sample is one latency test: when, and its delay (0: failed).
type Sample struct {
	At    time.Time
	Delay time.Duration
}

// NodeResult is everything known about one node's tests.
type NodeResult struct {
	Tested      bool
	Up          bool
	Latency     time.Duration // the last test's, when Up
	Connect     time.Duration // the last TCP connect's, when ConnectOK
	ConnectOK   bool
	Ok, Fail    uint64   // latency tests by result
	Buckets     []uint64 // per Buckets, plus +Inf last (not cumulative)
	Sum         float64  // seconds of the tests that passed
	History     []Sample // within the speed window
	LastSuccess time.Time
}

// SpeedResult is a group's last speed check.
type SpeedResult struct {
	Node   *node.Node
	Result string
	Rate   float64 // bytes per second after the first read (0 when it failed)
	TTFB   time.Duration
	Bytes  int64
	At     time.Time
}

// ProviderState is a provider's fetches.
type ProviderState struct {
	Nodes       int
	Excluded    int
	Duplicates  int
	Invalid     int
	Info        provider.Info
	FetchedAt   time.Time
	FetchErrors uint64
	LastError   string
}

// Raw is a provider's last good list as fetched, kept so a reload can rebuild its nodes with the
// new settings without fetching again.
type Raw struct {
	Source  string // its URL or file: a reload that changes it drops the provider instead
	Proxies []map[string]any
	Info    provider.Info
	At      time.Time
}

// Round is a probe round's duration and end.
type Round struct {
	Duration time.Duration
	At       time.Time
}

// Store is safe for concurrent use.
type Store struct {
	mu         sync.Mutex
	window     time.Duration
	nodes      map[string][]*node.Node // provider → its nodes
	raw        map[string]Raw
	results    map[string]*NodeResult  // node key → results
	speed      map[string]*SpeedResult // provider/group → last check
	speedTests map[[3]string]uint64    // provider, group, result → checks
	speedBytes map[string]uint64       // provider → bytes the checks downloaded
	speedOff   bool                    // speed checks turned off: no speed state at all
	providers  map[string]*ProviderState
	rounds     map[string]Round
}

// New is an empty store keeping window of latency history.
func New(window time.Duration) *Store {
	return &Store{
		window:     window,
		nodes:      map[string][]*node.Node{},
		raw:        map[string]Raw{},
		results:    map[string]*NodeResult{},
		speed:      map[string]*SpeedResult{},
		speedTests: map[[3]string]uint64{},
		speedBytes: map[string]uint64{},
		providers:  map[string]*ProviderState{},
		rounds:     map[string]Round{},
	}
}

// SetWindow changes how much latency history is kept (a config reload).
func (s *Store) SetWindow(w time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.window = w
}

// SetNodes replaces provider p's nodes, built from raw. The results of nodes that are gone go too
// (their series disappear); a node that stays (same provider and full name) keeps its results. It
// returns the nodes replaced, for the caller to close once no test uses them.
func (s *Store) SetNodes(p string, built *node.Built, raw Raw) []*node.Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.nodes[p]
	s.nodes[p] = built.Nodes
	s.raw[p] = raw
	keep := map[string]bool{}
	groups := map[string]bool{}
	for _, n := range built.Nodes {
		keep[n.Key()] = true
		groups[n.Group] = true
		if s.results[n.Key()] == nil {
			s.results[n.Key()] = &NodeResult{Buckets: make([]uint64, len(Buckets)+1)}
		}
	}
	for _, n := range old {
		if !keep[n.Key()] {
			delete(s.results, n.Key())
		}
	}
	st := s.provider(p)
	st.Nodes, st.Excluded, st.Duplicates, st.Invalid = len(built.Nodes), built.Excluded, built.Duplicates, len(built.Invalid)
	st.Info, st.FetchedAt, st.LastError = raw.Info, raw.At, ""
	// a group that is gone takes its speed check and counters with it
	for k, r := range s.speed {
		if r.Node.Provider == p && !groups[r.Node.Group] {
			delete(s.speed, k)
		}
	}
	for k := range s.speedTests {
		if k[0] == p && !groups[k[1]] {
			delete(s.speedTests, k)
		}
	}
	s.seedSpeed(p, groups)
	return old
}

// seedSpeed starts provider p's speed counters at 0, before any check: increase() over a window
// counts a counter's first value only as a change from an earlier sample, so a counter that appeared
// at 1 would leave the first check out of every rate and of the traffic.
func (s *Store) seedSpeed(p string, groups map[string]bool) {
	if s.speedOff {
		return
	}
	for g := range groups {
		for _, res := range []string{Pass, Slow, Fail} {
			if _, ok := s.speedTests[[3]string{p, g, res}]; !ok {
				s.speedTests[[3]string{p, g, res}] = 0
			}
		}
	}
	if _, ok := s.speedBytes[p]; !ok {
		s.speedBytes[p] = 0
	}
}

// Raw is provider p's last good list.
func (s *Store) Raw(p string) (Raw, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.raw[p]
	return r, ok
}

// FetchFailed records a failed fetch of provider p (its last list stays).
func (s *Store) FetchFailed(p string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.provider(p)
	st.FetchErrors++
	st.LastError = err.Error()
}

// DropProvider forgets provider p and returns its nodes to close.
func (s *Store) DropProvider(p string) []*node.Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.nodes[p]
	for _, n := range old {
		delete(s.results, n.Key())
	}
	for k, r := range s.speed {
		if r.Node.Provider == p {
			delete(s.speed, k)
		}
	}
	for k := range s.speedTests {
		if k[0] == p {
			delete(s.speedTests, k)
		}
	}
	delete(s.speedBytes, p)
	delete(s.nodes, p)
	delete(s.raw, p)
	delete(s.providers, p)
	return old
}

// SetSpeed turns speed checks on (the default: every provider's counters start at 0) or off (every
// speed check and counter is forgotten, and none start).
func (s *Store) SetSpeed(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.speedOff = !on
	if on {
		for p, ns := range s.nodes {
			groups := map[string]bool{}
			for _, n := range ns {
				groups[n.Group] = true
			}
			s.seedSpeed(p, groups)
		}
		return
	}
	s.speed = map[string]*SpeedResult{}
	s.speedTests = map[[3]string]uint64{}
	s.speedBytes = map[string]uint64{}
	delete(s.rounds, "speed")
}

// Providers is the names of the providers the store knows.
func (s *Store) Providers() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.providers))
	for p := range s.providers {
		out = append(out, p)
	}
	return out
}

func (s *Store) provider(p string) *ProviderState {
	st := s.providers[p]
	if st == nil {
		st = &ProviderState{}
		s.providers[p] = st
	}
	return st
}

// Nodes is every provider's current nodes.
func (s *Store) Nodes() []*node.Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*node.Node
	for _, ns := range s.nodes {
		out = append(out, ns...)
	}
	return out
}

// RecordLatency stores a latency test (delay 0: failed) and a connect test (connectOK false:
// failed or not run).
func (s *Store) RecordLatency(n *node.Node, at time.Time, delay, connect time.Duration, connectOK bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.results[n.Key()]
	if r == nil { // the node went while it was tested
		return
	}
	r.Tested = true
	r.Up = delay > 0
	r.Latency = delay
	r.Connect, r.ConnectOK = connect, connectOK
	if r.Up {
		r.Ok++
		sec := delay.Seconds()
		r.Buckets[sort.SearchFloat64s(Buckets, sec)]++ // the first bound ≥ sec, or +Inf
		r.Sum += sec
		r.LastSuccess = at
	} else {
		r.Fail++
	}
	r.History = append(trim(r.History, at.Add(-s.window)), Sample{At: at, Delay: delay})
}

func trim(h []Sample, after time.Time) []Sample {
	i := 0
	for i < len(h) && !h[i].At.After(after) {
		i++
	}
	return append(h[:0], h[i:]...)
}

// Pick is, for each (provider, group), the node to speed test: the lowest average delay over the
// window, a failed test counting as timeout; when every member failed throughout (or has no test
// yet), the one that last passed, else the first by label.
func (s *Store) Pick(now time.Time, timeout time.Duration) []*node.Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	type cand struct {
		n   *node.Node
		avg time.Duration
		ok  time.Time
	}
	picks := map[string]*cand{}
	for _, ns := range s.nodes {
		for _, n := range ns {
			c := &cand{n: n, avg: timeout}
			if r := s.results[n.Key()]; r != nil {
				var sum time.Duration
				var cnt int
				for _, sm := range r.History {
					if sm.At.After(now.Add(-s.window)) {
						cnt++
						if sm.Delay > 0 {
							sum += sm.Delay
						} else {
							sum += timeout
						}
					}
				}
				if cnt > 0 {
					c.avg = sum / time.Duration(cnt)
				}
				c.ok = r.LastSuccess
			}
			k := n.Provider + "/" + n.Group
			if b := picks[k]; b == nil || better(c.avg, c.ok, c.n.Label, b.avg, b.ok, b.n.Label, timeout) {
				picks[k] = c
			}
		}
	}
	keys := make([]string, 0, len(picks))
	for k := range picks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*node.Node, 0, len(keys))
	for _, k := range keys {
		out = append(out, picks[k].n)
	}
	return out
}

// better orders speed-test candidates: the lower average; among those that only failed, the one
// that passed last; then the label (deterministic whatever the map order).
func better(avgA time.Duration, okA time.Time, labelA string, avgB time.Duration, okB time.Time, labelB string, timeout time.Duration) bool {
	if avgA != avgB {
		return avgA < avgB
	}
	if avgA >= timeout && !okA.Equal(okB) {
		return okA.After(okB)
	}
	return labelA < labelB
}

// RecordSpeed stores a group's speed check, unless its node went while it was tested (or speed checks
// were turned off meanwhile).
func (s *Store) RecordSpeed(r *SpeedResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := r.Node
	if s.results[n.Key()] == nil || s.speedOff {
		return
	}
	s.speed[n.Provider+"/"+n.Group] = r
	s.speedTests[[3]string{n.Provider, n.Group, r.Result}]++
	if r.Bytes > 0 {
		s.speedBytes[n.Provider] += uint64(r.Bytes)
	}
}

// RecordRound stores a round's duration.
func (s *Store) RecordRound(kind string, d time.Duration, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rounds[kind] = Round{d, at}
}

// NodeView is a node and a copy of its results.
type NodeView struct {
	Node   *node.Node
	Result NodeResult
}

// SpeedTest is a speed-check counter.
type SpeedTest struct {
	Provider, Group, Result string
	Count                   uint64
}

// Snapshot is a consistent copy of everything, for one scrape or status request.
type Snapshot struct {
	Nodes      []NodeView
	Speed      []SpeedResult
	SpeedTests []SpeedTest
	SpeedBytes map[string]uint64
	Providers  map[string]ProviderState
	Rounds     map[string]Round
}

// Snapshot copies the store.
func (s *Store) Snapshot() *Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	sn := &Snapshot{SpeedBytes: map[string]uint64{}, Providers: map[string]ProviderState{}, Rounds: map[string]Round{}}
	for _, ns := range s.nodes {
		for _, n := range ns {
			r := s.results[n.Key()]
			if r == nil {
				continue
			}
			c := *r
			c.Buckets = append([]uint64(nil), r.Buckets...)
			c.History = nil
			sn.Nodes = append(sn.Nodes, NodeView{n, c})
		}
	}
	sort.Slice(sn.Nodes, func(i, j int) bool {
		a, b := sn.Nodes[i].Node, sn.Nodes[j].Node
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.Label < b.Label
	})
	for _, r := range s.speed {
		sn.Speed = append(sn.Speed, *r)
	}
	sort.Slice(sn.Speed, func(i, j int) bool {
		a, b := sn.Speed[i].Node, sn.Speed[j].Node
		return a.Provider+"/"+a.Group < b.Provider+"/"+b.Group
	})
	for k, v := range s.speedTests {
		sn.SpeedTests = append(sn.SpeedTests, SpeedTest{k[0], k[1], k[2], v})
	}
	for k, v := range s.speedBytes {
		sn.SpeedBytes[k] = v
	}
	for k, v := range s.providers {
		sn.Providers[k] = *v
	}
	for k, v := range s.rounds {
		sn.Rounds[k] = v
	}
	return sn
}
