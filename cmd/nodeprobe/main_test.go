package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/0xha0/nodeprobe/internal/config"
	"github.com/0xha0/nodeprobe/internal/store"
)

func TestHealthy(t *testing.T) {
	cfg := &config.Config{Latency: config.Latency{Interval: config.Duration{Duration: time.Minute}}}
	now := time.Now()
	none := &store.Snapshot{Rounds: map[string]store.Round{}}
	recent := &store.Snapshot{Rounds: map[string]store.Round{"latency": {At: now.Add(-2 * time.Minute)}}}
	stale := &store.Snapshot{Rounds: map[string]store.Round{"latency": {At: now.Add(-4 * time.Minute)}}}
	cases := []struct {
		name    string
		sn      *store.Snapshot
		started time.Time
		ok      bool
	}{
		{"starting", none, now.Add(-time.Minute), true},
		{"never ran", none, now.Add(-time.Hour), false},
		{"recent round", recent, now.Add(-time.Hour), true},
		{"stale round", stale, now.Add(-time.Hour), false},
	}
	for _, c := range cases {
		if err := healthy(c.sn, cfg, c.started, now); (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

func TestLocalHealthz(t *testing.T) {
	dir := t.TempDir()
	f := dir + "/c.yaml"
	if err := os.WriteFile(f, []byte("listen: ':9200'\nproviders: [{name: a, url: 'http://h/'}]\ngroups: [{name: X, match: x}]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := localHealthz(f); got != "http://127.0.0.1:9200/healthz" {
		t.Errorf("%s", got)
	}
	if got := localHealthz(dir + "/missing.yaml"); got != "http://127.0.0.1:9101/healthz" {
		t.Errorf("%s", got)
	}
}

func TestHealthcheck(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer ok.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer bad.Close()
	if healthcheck(ok.URL) != 0 || healthcheck(bad.URL) != 1 || healthcheck("http://127.0.0.1:1/") != 1 {
		t.Error("healthcheck exit codes")
	}
}
