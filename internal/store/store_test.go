package store

import (
	"testing"
	"time"

	"github.com/0xha0/nodeprobe/internal/node"
	"github.com/0xha0/nodeprobe/internal/provider"
)

func nodes(provider, group string, labels ...string) *node.Built {
	b := &node.Built{}
	for _, l := range labels {
		b.Nodes = append(b.Nodes, &node.Node{Provider: provider, Name: "full " + l, Label: l, Group: group})
	}
	return b
}

func TestRecordLatency(t *testing.T) {
	s := New(30 * time.Minute)
	b := nodes("p", "HK", "a")
	s.SetNodes("p", b, Raw{At: time.Now()})
	n := b.Nodes[0]
	t0 := time.Now()
	s.RecordLatency(n, t0, 120*time.Millisecond, 90*time.Millisecond, true)
	s.RecordLatency(n, t0.Add(time.Minute), 0, 0, false)
	s.RecordLatency(n, t0.Add(2*time.Minute), 7*time.Second, 0, true)
	r := s.Snapshot().Nodes[0].Result
	if r.Ok != 2 || r.Fail != 1 || r.Up != true || r.Latency != 7*time.Second {
		t.Errorf("%+v", r)
	}
	if r.Buckets[2] != 1 || r.Buckets[len(Buckets)] != 1 { // 0.12 in le=0.15; 7 s past 5 s: +Inf
		t.Errorf("buckets %v", r.Buckets)
	}
	if r.Sum < 7.11 || r.Sum > 7.13 {
		t.Errorf("sum %v", r.Sum)
	}
	// history is trimmed to the window
	s.RecordLatency(n, t0.Add(time.Hour), 100*time.Millisecond, 0, false)
	if h := s.results[n.Key()].History; len(h) != 1 {
		t.Errorf("history not trimmed: %v", h)
	}
}

func TestSetNodesKeepsAndDrops(t *testing.T) {
	s := New(time.Hour)
	b1 := nodes("p", "HK", "a", "b")
	s.SetNodes("p", b1, Raw{At: time.Now()})
	s.RecordLatency(b1.Nodes[0], time.Now(), time.Millisecond, 0, false)
	s.RecordSpeed(&SpeedResult{Node: b1.Nodes[0], Result: Pass, Rate: 1, Bytes: 10, At: time.Now()})
	old := s.SetNodes("p", nodes("p", "JP", "a", "c"), Raw{Info: provider.Info{Total: 5}, At: time.Now()}) // b gone, a stays, HK gone
	if len(old) != 2 {
		t.Errorf("old nodes %d", len(old))
	}
	sn := s.Snapshot()
	if len(sn.Nodes) != 2 || sn.Nodes[0].Result.Ok != 1 || sn.Nodes[1].Result.Tested {
		t.Errorf("a must keep its results, c start fresh: %+v", sn.Nodes)
	}
	if len(sn.Speed) != 0 || len(sn.SpeedTests) != 0 {
		t.Errorf("the speed check and counters of a group that is gone must go: %+v %+v", sn.Speed, sn.SpeedTests)
	}
	if sn.Providers["p"].Info.Total != 5 || sn.SpeedBytes["p"] != 10 {
		t.Errorf("%+v %+v", sn.Providers, sn.SpeedBytes)
	}
	s.DropProvider("p")
	if sn := s.Snapshot(); len(sn.Nodes)+len(sn.Providers)+len(sn.SpeedTests) != 0 {
		t.Errorf("drop left %+v", sn)
	}
}

func TestPick(t *testing.T) {
	s := New(30 * time.Minute)
	hk := nodes("p", "HK", "fast-flaky", "steady", "z-steady")
	jp := nodes("p", "JP", "dead1", "dead2", "dead3")
	all := &node.Built{Nodes: append(append([]*node.Node{}, hk.Nodes...), jp.Nodes...)}
	s.SetNodes("p", all, Raw{At: time.Now()})
	now := time.Now()
	to := 5 * time.Second
	ms := time.Millisecond
	// fast-flaky: 50 ms once, then a failure: (50 + 5000) / 2; steady: 120 twice; z-steady ties steady
	s.RecordLatency(hk.Nodes[0], now.Add(-10*time.Minute), 50*ms, 0, false)
	s.RecordLatency(hk.Nodes[0], now.Add(-5*time.Minute), 0, 0, false)
	for _, n := range hk.Nodes[1:] {
		s.RecordLatency(n, now.Add(-10*time.Minute), 120*ms, 0, false)
		s.RecordLatency(n, now.Add(-5*time.Minute), 120*ms, 0, false)
	}
	// JP: all fail now; dead2 passed an hour ago (outside the window), dead1 never
	s.RecordLatency(jp.Nodes[1], now.Add(-time.Hour), 80*ms, 0, false)
	for _, n := range jp.Nodes {
		s.RecordLatency(n, now.Add(-time.Minute), 0, 0, false)
	}
	got := s.Pick(now, to)
	if len(got) != 2 || got[0].Label != "steady" || got[1].Label != "dead2" {
		t.Errorf("picks %v %v", label(got, 0), label(got, 1))
	}
}

func label(ns []*node.Node, i int) string {
	if i < len(ns) {
		return ns[i].Label
	}
	return "-"
}

func TestSpeedCounters(t *testing.T) {
	s := New(time.Hour)
	b := nodes("p", "HK", "a")
	s.SetNodes("p", b, Raw{At: time.Now()})
	s.RecordSpeed(&SpeedResult{Node: b.Nodes[0], Result: Slow, At: time.Now()})
	got := map[string]uint64{}
	for _, t := range s.Snapshot().SpeedTests {
		got[t.Result] = t.Count
	}
	if len(got) != 3 || got[Slow] != 1 || got[Pass] != 0 || got[Fail] != 0 {
		t.Errorf("all three results from the first check: %v", got)
	}
}

func TestRecordSpeedOfAGoneNode(t *testing.T) {
	s := New(time.Hour)
	b := nodes("p", "HK", "a")
	s.SetNodes("p", b, Raw{At: time.Now()})
	s.SetNodes("p", nodes("p", "HK", "b"), Raw{At: time.Now()}) // a goes while its check runs
	s.RecordSpeed(&SpeedResult{Node: b.Nodes[0], Result: Pass, At: time.Now()})
	if sn := s.Snapshot(); len(sn.Speed)+len(sn.SpeedTests) != 0 {
		t.Errorf("a check of a node that is gone came back: %+v", sn.Speed)
	}
}

func TestClearSpeedAndRaw(t *testing.T) {
	s := New(time.Hour)
	b := nodes("p", "HK", "a")
	s.SetNodes("p", b, Raw{Source: "u", At: time.Now()})
	s.RecordSpeed(&SpeedResult{Node: b.Nodes[0], Result: Pass, Bytes: 5, At: time.Now()})
	s.RecordRound("speed", time.Second, time.Now())
	s.ClearSpeed()
	sn := s.Snapshot()
	if len(sn.Speed)+len(sn.SpeedTests)+len(sn.SpeedBytes) != 0 || sn.Rounds["speed"] != (Round{}) {
		t.Errorf("speed state left: %+v", sn)
	}
	if r, ok := s.Raw("p"); !ok || r.Source != "u" {
		t.Errorf("raw %+v %v", r, ok)
	}
}
