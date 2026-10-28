package probe

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metacubex/mihomo/adapter"
	C "github.com/metacubex/mihomo/constant"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/0xha0/nodeprobe/internal/config"
	"github.com/0xha0/nodeprobe/internal/metrics"
	"github.com/0xha0/nodeprobe/internal/store"
)

// fakeProxy is an HTTP CONNECT proxy (what mihomo's `type: http` outbound speaks). delay holds
// back every tunnel's set-up.
type fakeProxy struct {
	ln       net.Listener
	connects atomic.Int64
	delay    time.Duration
}

func newFakeProxy(t *testing.T) *fakeProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeProxy{ln: ln}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeProxy) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeProxy) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil || req.Method != http.MethodConnect {
		return
	}
	f.connects.Add(1)
	time.Sleep(f.delay)
	up, err := net.Dial("tcp", req.Host)
	if err != nil {
		_, _ = c.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer func() { _ = up.Close() }()
	_, _ = c.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
	go func() { _, _ = io.Copy(up, br) }()
	_, _ = io.Copy(c, up)
}

const ua = "nodeprobe/test"

func closedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return p
}

func httpNode(t *testing.T, name string, port int) C.Proxy {
	t.Helper()
	p, err := adapter.ParseProxy(map[string]any{"name": name, "type": "http", "server": "127.0.0.1", "port": port})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// target serves /204, /403, /__down?bytes=N (throttled by ?slow=1) and checks the User-Agent.
func target(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.UserAgent(), "nodeprobe") {
			http.Error(w, "agent", http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/204":
			w.WriteHeader(http.StatusNoContent)
		case "/403":
			w.WriteHeader(http.StatusForbidden)
		case "/__down":
			n, _ := strconv.Atoi(r.URL.Query().Get("bytes"))
			chunk := []byte(strings.Repeat("x", 16<<10))
			for sent := 0; sent < n; sent += len(chunk) {
				if _, err := w.Write(chunk[:min(len(chunk), n-sent)]); err != nil {
					return
				}
				if r.URL.Query().Get("slow") == "1" {
					w.(http.Flusher).Flush()
					time.Sleep(20 * time.Millisecond)
				}
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func ctx(t *testing.T, d time.Duration) context.Context {
	c, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return c
}

func TestLatencyUnifiedReusesTheConnection(t *testing.T) {
	tg := target(t)
	px := newFakeProxy(t)
	px.delay = 100 * time.Millisecond // set-up only: the unified time leaves it out
	n := httpNode(t, "n", px.port())
	d, err := Latency(ctx(t, 5*time.Second), n, tg.URL+"/204", ua, true)
	if err != nil {
		t.Fatal(err)
	}
	if px.connects.Load() != 1 {
		t.Errorf("unified made %d tunnels, want 1", px.connects.Load())
	}
	if d >= 100*time.Millisecond {
		t.Errorf("unified delay %v includes the set-up", d)
	}
	cold, err := Latency(ctx(t, 5*time.Second), n, tg.URL+"/204", ua, false)
	if err != nil || cold < 100*time.Millisecond {
		t.Errorf("first-request delay %v %v must include the set-up", cold, err)
	}
}

func TestLatencyFailures(t *testing.T) {
	tg := target(t)
	px := newFakeProxy(t)
	if _, err := Latency(ctx(t, 5*time.Second), httpNode(t, "n", px.port()), tg.URL+"/403", ua, true); err == nil {
		t.Error("a 403 passed")
	}
	if _, err := Latency(ctx(t, 5*time.Second), httpNode(t, "dead", closedPort(t)), tg.URL+"/204", ua, true); err == nil {
		t.Error("a dead node passed")
	}
	px.delay = time.Second
	start := time.Now()
	if _, err := Latency(ctx(t, 200*time.Millisecond), httpNode(t, "slow", px.port()), tg.URL+"/204", ua, true); err == nil || time.Since(start) > 900*time.Millisecond {
		t.Errorf("the timeout was not kept: %v after %v", err, time.Since(start))
	}
}

func TestSpeed(t *testing.T) {
	tg := target(t)
	px := newFakeProxy(t)
	n := httpNode(t, "n", px.port())
	r, err := Speed(ctx(t, 5*time.Second), n, tg.URL+"/__down?bytes={bytes}", ua, 300_000)
	if err != nil || r.Bytes != 300_000 || r.Rate <= 0 || r.TTFB <= 0 {
		t.Fatalf("%+v %v", r, err)
	}
	// the deadline cuts a slow download: what arrived still counts
	r, err = Speed(ctx(t, 300*time.Millisecond), n, tg.URL+"/__down?bytes={bytes}&slow=1", ua, 10_000_000)
	if err != nil || r.Bytes == 0 || r.Bytes >= 10_000_000 {
		t.Fatalf("cut download: %+v %v", r, err)
	}
	if _, err := Speed(ctx(t, 5*time.Second), httpNode(t, "dead", closedPort(t)), tg.URL+"/__down?bytes={bytes}", ua, 1000); err == nil {
		t.Error("a dead node delivered")
	}
}

// TestRunner runs the whole thing: a provider served with a key and subscription-userinfo, a
// latency and a speed round, the metrics; the key is never in the metrics, the status or the logs.
func TestRunner(t *testing.T) {
	tg := target(t)
	good, slow := newFakeProxy(t), newFakeProxy(t)
	slow.delay = 150 * time.Millisecond
	dead := closedPort(t)
	list := fmt.Sprintf(`proxies:
  - {name: "🇭🇰 HK | 香港 01", type: http, server: 127.0.0.1, port: %d}
  - {name: "🇭🇰 HK | 香港 02", type: http, server: 127.0.0.1, port: %d}
  - {name: "🇯🇵 JP | 日本 01", type: http, server: 127.0.0.1, port: %d}
  - {name: "到期 2099-01-01", type: http, server: 127.0.0.1, port: 1}
`, good.port(), slow.port(), dead)
	sub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("key") != "SECRETKEY" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Subscription-Userinfo", "upload=10; download=20; total=1000; expire=1900000000")
		_, _ = io.WriteString(w, list)
	}))
	t.Cleanup(sub.Close)
	cfg, err := config.Parse([]byte(`
vantage: test
providers:
  - {name: main, url: "${SUB}", exclude: "到期"}
groups:
  - {name: HK, match: "🇭🇰"}
  - {name: Other, match: "."}
latency: {url: "${TG}/204", timeout: 2s, interval: 1m, concurrency: 2, unified: false}
speed: {url: "${TG}/__down?bytes={bytes}", bytes: 200KB, timeout: 3s, window: 5m}
`), func(k string) (string, bool) {
		return map[string]string{"SUB": sub.URL + "/s?key=SECRETKEY", "TG": tg.URL}[k], true
	})
	if err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(writerFunc(func(b []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return logs.Write(b) }), &slog.HandlerOptions{Level: slog.LevelDebug}))
	st := store.New(cfg.Speed.Window.Duration)
	r := NewRunner(cfg, st, log, ua)
	c := ctx(t, 30*time.Second)
	if err := r.Fetch(c, &cfg.Providers[0]); err != nil {
		t.Fatal(err)
	}
	r.LatencyRound(c)
	r.LatencyRound(c)
	r.SpeedRound(c)

	reg := prometheus.NewRegistry()
	reg.MustRegister(&metrics.Collector{Store: st, Vantage: func() string { return cfg.Vantage }, Version: "t", Core: "c"})
	for _, want := range []string{
		`nodeprobe_node_up{entry="127.0.0.1",group="HK",node="HK 01",provider="main",vantage="test"} 1`,
		`nodeprobe_node_up{entry="127.0.0.1",group="Other",node="JP 01",provider="main",vantage="test"} 0`,
		`nodeprobe_node_checks_total{entry="127.0.0.1",group="Other",node="JP 01",provider="main",result="fail",vantage="test"} 2`,
		`nodeprobe_node_checks_total{entry="127.0.0.1",group="Other",node="JP 01",provider="main",result="ok",vantage="test"} 0`,
		`nodeprobe_speed_result{entry="127.0.0.1",group="HK",node="HK 01",provider="main",vantage="test"} 1`,
		`nodeprobe_speed_result{entry="127.0.0.1",group="Other",node="JP 01",provider="main",vantage="test"} 0`,
		`nodeprobe_speed_tests_total{group="HK",provider="main",result="pass",vantage="test"} 1`,
		`nodeprobe_provider_nodes{provider="main",vantage="test"} 3`,
		`nodeprobe_provider_excluded_nodes{provider="main",vantage="test"} 1`,
		`nodeprobe_provider_traffic_used_bytes{provider="main",vantage="test"} 30`,
		`nodeprobe_provider_traffic_total_bytes{provider="main",vantage="test"} 1000`,
		`nodeprobe_provider_expiry_timestamp_seconds{provider="main",vantage="test"} 1.9e+09`,
		`nodeprobe_speed_bytes_total{provider="main",vantage="test"} 200000`,
		`nodeprobe_node_connect_seconds{entry="127.0.0.1",group="HK",node="HK 01",provider="main",vantage="test"}`,
		`nodeprobe_latency_seconds_bucket{entry="127.0.0.1",group="HK",node="HK 01",provider="main",vantage="test",le="+Inf"} 2`,
		`nodeprobe_latency_seconds_count{entry="127.0.0.1",group="HK",node="HK 01",provider="main",vantage="test"} 2`,
	} {
		if !strings.Contains(scrape(t, reg), want) {
			t.Errorf("missing: %s", want)
		}
	}
	if strings.Contains(scrape(t, reg), `nodeprobe_node_connect_seconds{entry="127.0.0.1",group="Other",node="JP 01"`) {
		t.Error("a connect to a closed port reported a time")
	}
	// a histogram for every tested node (JP 01's empty); HK 02's set-up costs 150 ms (counted:
	// unified is off), so HK 01 was the group's speed-test node (checked above)
	if n := testutil.CollectAndCount(&metrics.Collector{Store: st, Vantage: func() string { return "test" }}, "nodeprobe_latency_seconds"); n != 3 {
		t.Errorf("histograms: %d, want 3", n)
	}
	rec := httptest.NewRecorder()
	metrics.Status(st, func() string { return "test" }).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	for what, text := range map[string]string{"metrics": scrape(t, reg), "status": rec.Body.String(), "logs": logs.String()} {
		if strings.Contains(text, "SECRETKEY") || strings.Contains(text, "key=") {
			t.Errorf("the subscription key is in the %s", what)
		}
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

func scrape(t *testing.T, reg *prometheus.Registry) string {
	t.Helper()
	srv := httptest.NewServer(promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// a provider config and a fake list of the given nodes (name → fake proxy port)
func runnerFor(t *testing.T, tg *httptest.Server, extra string, nodes map[string]int) (*config.Config, *Runner, *store.Store) {
	t.Helper()
	var list strings.Builder
	list.WriteString("proxies:\n")
	for name, port := range nodes {
		fmt.Fprintf(&list, "  - {name: %q, type: http, server: 127.0.0.1, port: %d}\n", name, port)
	}
	file := t.TempDir() + "/list.yaml"
	if err := os.WriteFile(file, []byte(list.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
providers: [{name: main, file: %q}]
groups: [{name: All, match: "."}]
latency: {url: "%s/204", timeout: 2s, interval: 1m}
speed: {url: "%s/__down?bytes={bytes}&slow=1", bytes: 100KB, timeout: 3s, window: 5m}
%s`, file, tg.URL, tg.URL, extra)), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	st := store.New(cfg.Speed.Window.Duration)
	r := NewRunner(cfg, st, slog.New(slog.DiscardHandler), ua)
	if err := r.Fetch(ctx(t, 10*time.Second), &cfg.Providers[0]); err != nil {
		t.Fatal(err)
	}
	return cfg, r, st
}

func TestSlowSpeed(t *testing.T) {
	tg := target(t)
	px := newFakeProxy(t)
	// the slow download sends 16 KiB every 20 ms (~6.5 Mbps): below 50 Mbps is slow
	_, r, st := runnerFor(t, tg, "", map[string]int{"n": px.port()})
	r.cfg.Speed.MinRate = 50_000_000
	r.LatencyRound(ctx(t, 10*time.Second))
	r.SpeedRound(ctx(t, 10*time.Second))
	sp := st.Snapshot().Speed
	if len(sp) != 1 || sp[0].Result != store.Slow || sp[0].Rate <= 0 {
		t.Errorf("%+v", sp)
	}
}

// TestRebuild: a new runner (a reload) rebuilds the nodes from the last list with its own groups at
// once; a changed source drops the provider.
func TestRebuild(t *testing.T) {
	tg := target(t)
	px := newFakeProxy(t)
	cfg, _, st := runnerFor(t, tg, "", map[string]int{"🇭🇰 HK 01": px.port()})
	next := *cfg
	next.Groups = []config.Group{{Name: "HK", Match: config.MustRegexp("🇭🇰")}}
	NewRunner(&next, st, slog.New(slog.DiscardHandler), ua).Rebuild()
	if n := st.Nodes(); len(n) != 1 || n[0].Group != "HK" {
		t.Errorf("not rebuilt with the new groups: %+v", n)
	}
	moved := next
	moved.Providers = []config.Provider{next.Providers[0]}
	moved.Providers[0].File = "/elsewhere.yaml"
	NewRunner(&moved, st, slog.New(slog.DiscardHandler), ua).Rebuild()
	if n := st.Nodes(); len(n) != 0 {
		t.Errorf("a provider whose source changed must be dropped: %+v", n)
	}
}

// TestCloseAfterRound: outbounds replaced by a fetch are closed when the round testing them ends,
// not before.
func TestCloseAfterRound(t *testing.T) {
	tg := target(t)
	px := newFakeProxy(t)
	cfg, r, st := runnerFor(t, tg, "", map[string]int{"n": px.port()})
	old := st.Nodes()
	r.busy.Lock() // a round in progress
	if err := r.Fetch(ctx(t, 10*time.Second), &cfg.Providers[0]); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := Latency(ctx(t, 5*time.Second), old[0].Proxy, tg.URL+"/204", ua, true); err != nil {
		t.Errorf("the old outbound was closed during the round: %v", err)
	}
	r.busy.Unlock()
	r.closing.Wait()
	if st.Nodes()[0] == old[0] {
		t.Error("the fetch did not replace the node")
	}
}
