// Package metrics exports the store as Prometheus metrics and a JSON status.
package metrics

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/0xha0/nodeprobe/internal/store"
)

var (
	nodeLabels  = []string{"vantage", "provider", "group", "node", "entry"}
	provLabels  = []string{"vantage", "provider"}
	groupLabels = []string{"vantage", "provider", "group"}

	descs []*prometheus.Desc
)

func desc(name, help string, labels ...string) *prometheus.Desc {
	d := prometheus.NewDesc("nodeprobe_"+name, help, labels, nil)
	descs = append(descs, d)
	return d
}

func with(labels []string, more ...string) []string {
	return append(append([]string{}, labels...), more...)
}

var (
	dNodeUp      = desc("node_up", "1 when the node passed its last latency test.", nodeLabels...)
	dNodeLatency = desc("node_last_latency_seconds", "The node's last latency test (absent when it failed).", nodeLabels...)
	dLatency     = desc("latency_seconds", "Histogram of the node's latency tests that passed.", nodeLabels...)
	dChecks      = desc("node_checks_total", "Latency tests by result (ok, fail); both series from the start.", with(nodeLabels, "result")...)
	dConnect     = desc("node_connect_seconds", "TCP connect to the node's entry server, DNS excluded (absent when it failed or wasn't run: UDP protocols).", nodeLabels...)

	dSpeedResult = desc("speed_result", "A group's last speed check: 1 pass, 0.5 slow, 0 fail; node is the one tested.", nodeLabels...)
	dSpeedRate   = desc("speed_bytes_per_second", "A group's last speed check: bytes per second after the first read (0: failed).", nodeLabels...)
	dSpeedTTFB   = desc("speed_ttfb_seconds", "A group's last speed check: request to first read (absent when it failed).", nodeLabels...)
	dSpeedAt     = desc("speed_timestamp_seconds", "When a group was last speed checked.", nodeLabels...)
	dSpeedTests  = desc("speed_tests_total", "Speed checks by group and result (pass, slow, fail).", with(groupLabels, "result")...)
	dSpeedBytes  = desc("speed_bytes_total", "Bytes the speed checks downloaded (the traffic they cost).", provLabels...)

	dProvNodes     = desc("provider_nodes", "Nodes tested from the provider's list.", provLabels...)
	dProvExcluded  = desc("provider_excluded_nodes", "Entries of the list left out by include / exclude.", provLabels...)
	dProvDuplicate = desc("provider_duplicate_nodes", "Entries whose name came earlier in the list (tested as <name> #2, …).", provLabels...)
	dProvInvalid   = desc("provider_invalid_nodes", "Entries left out: an info entry (no real server), not a remote proxy, dialer-proxy, or mihomo could not build them.", provLabels...)
	dProvFetched   = desc("provider_fetch_timestamp_seconds", "When the list was last fetched successfully.", provLabels...)
	dProvErrors    = desc("provider_fetch_errors_total", "Failed fetches (the last good list stays).", provLabels...)
	dProvUsed      = desc("provider_traffic_used_bytes", "Traffic used, from the provider's subscription-userinfo.", provLabels...)
	dProvTotal     = desc("provider_traffic_total_bytes", "Traffic allowed, from the provider's subscription-userinfo.", provLabels...)
	dProvExpiry    = desc("provider_expiry_timestamp_seconds", "When the subscription expires, from its subscription-userinfo.", provLabels...)

	dBuild   = desc("build_info", "nodeprobe's version and the mihomo (Clash Meta) core's.", "vantage", "version", "core")
	dRound   = desc("round_seconds", "How long the last round took.", "vantage", "kind")
	dRoundAt = desc("round_timestamp_seconds", "When the last round ended.", "vantage", "kind")
)

// Collector reads the store at each scrape.
type Collector struct {
	Store   *store.Store
	Vantage func() string // the current config's (it can change on a reload)
	Version string
	Core    string
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range descs {
		ch <- d
	}
}

var speedValue = map[string]float64{store.Pass: 1, store.Slow: 0.5, store.Fail: 0}

// Collect implements prometheus.Collector. A metric that can't be made (it shouldn't happen: labels
// are made valid UTF-8 when nodes are built) is reported as invalid on its own, the rest go out.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	v := c.Vantage()
	sn := c.Store.Snapshot()
	send := func(m prometheus.Metric, err error, d *prometheus.Desc) {
		if err != nil {
			m = prometheus.NewInvalidMetric(d, err)
		}
		ch <- m
	}
	gauge := func(d *prometheus.Desc, val float64, labels ...string) {
		m, err := prometheus.NewConstMetric(d, prometheus.GaugeValue, val, labels...)
		send(m, err, d)
	}
	counter := func(d *prometheus.Desc, val float64, labels ...string) {
		m, err := prometheus.NewConstMetric(d, prometheus.CounterValue, val, labels...)
		send(m, err, d)
	}
	gauge(dBuild, 1, v, c.Version, c.Core)

	for _, nv := range sn.Nodes {
		n, r := nv.Node, nv.Result
		l := []string{v, n.Provider, n.Group, n.Label, n.Server}
		counter(dChecks, float64(r.Ok), with(l, "ok")...)
		counter(dChecks, float64(r.Fail), with(l, "fail")...)
		if !r.Tested {
			continue
		}
		up := 0.0
		if r.Up {
			up = 1
			gauge(dNodeLatency, r.Latency.Seconds(), l...)
		}
		gauge(dNodeUp, up, l...)
		if r.ConnectOK {
			gauge(dConnect, r.Connect.Seconds(), l...)
		}
		buckets := make(map[float64]uint64, len(store.Buckets))
		var cum uint64
		for i, b := range store.Buckets {
			cum += r.Buckets[i]
			buckets[b] = cum
		}
		cum += r.Buckets[len(store.Buckets)]
		m, err := prometheus.NewConstHistogram(dLatency, cum, r.Sum, buckets, l...)
		send(m, err, dLatency)
	}

	for _, s := range sn.Speed {
		n := s.Node
		l := []string{v, n.Provider, n.Group, n.Label, n.Server}
		gauge(dSpeedResult, speedValue[s.Result], l...)
		gauge(dSpeedRate, s.Rate, l...)
		if s.Result != store.Fail {
			gauge(dSpeedTTFB, s.TTFB.Seconds(), l...)
		}
		gauge(dSpeedAt, float64(s.At.Unix()), l...)
	}
	for _, t := range sn.SpeedTests {
		counter(dSpeedTests, float64(t.Count), v, t.Provider, t.Group, t.Result)
	}
	for p, b := range sn.SpeedBytes {
		counter(dSpeedBytes, float64(b), v, p)
	}

	for p, st := range sn.Providers {
		counter(dProvErrors, float64(st.FetchErrors), v, p)
		if st.FetchedAt.IsZero() {
			continue
		}
		gauge(dProvNodes, float64(st.Nodes), v, p)
		gauge(dProvExcluded, float64(st.Excluded), v, p)
		gauge(dProvDuplicate, float64(st.Duplicates), v, p)
		gauge(dProvInvalid, float64(st.Invalid), v, p)
		gauge(dProvFetched, float64(st.FetchedAt.Unix()), v, p)
		if st.Info.Total > 0 || st.Info.Upload > 0 || st.Info.Download > 0 {
			gauge(dProvUsed, float64(st.Info.Upload+st.Info.Download), v, p)
		}
		if st.Info.Total > 0 {
			gauge(dProvTotal, float64(st.Info.Total), v, p)
		}
		if !st.Info.Expire.IsZero() {
			gauge(dProvExpiry, float64(st.Info.Expire.Unix()), v, p)
		}
	}
	for k, r := range sn.Rounds {
		gauge(dRound, r.Duration.Seconds(), v, k)
		gauge(dRoundAt, float64(r.At.Unix()), v, k)
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// Status is /api/status: the last results as JSON (no subscription URL ever).
func Status(st *store.Store, vantage func() string) http.Handler {
	type nodeJSON struct {
		Provider  string   `json:"provider"`
		Group     string   `json:"group"`
		Node      string   `json:"node"`
		Name      string   `json:"name"`
		Entry     string   `json:"entry"`
		Tested    bool     `json:"tested"`
		Up        bool     `json:"up"`
		LatencyMs *float64 `json:"latency_ms,omitempty"`
		ConnectMs *float64 `json:"connect_ms,omitempty"`
		Ok        uint64   `json:"ok"`
		Fail      uint64   `json:"fail"`
	}
	type speedJSON struct {
		Provider string    `json:"provider"`
		Group    string    `json:"group"`
		Node     string    `json:"node"`
		Result   string    `json:"result"`
		Mbps     float64   `json:"mbps"`
		TTFBMs   float64   `json:"ttfb_ms"`
		At       time.Time `json:"at"`
	}
	type provJSON struct {
		Name        string    `json:"name"`
		Nodes       int       `json:"nodes"`
		Excluded    int       `json:"excluded"`
		Duplicates  int       `json:"duplicates"`
		Invalid     int       `json:"invalid"`
		FetchedAt   time.Time `json:"fetched_at,omitzero"`
		FetchErrors uint64    `json:"fetch_errors"`
		LastError   string    `json:"last_error,omitempty"`
		Info        any       `json:"subscription,omitempty"`
	}
	type roundJSON struct {
		Ms float64   `json:"ms"`
		At time.Time `json:"at"`
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sn := st.Snapshot()
		out := struct {
			Vantage   string               `json:"vantage"`
			Providers []provJSON           `json:"providers"`
			Nodes     []nodeJSON           `json:"nodes"`
			Speed     []speedJSON          `json:"speed"`
			Rounds    map[string]roundJSON `json:"rounds"`
		}{Vantage: vantage(), Rounds: map[string]roundJSON{}, Nodes: []nodeJSON{}, Speed: []speedJSON{}, Providers: []provJSON{}}
		for name, p := range sn.Providers {
			pj := provJSON{Name: name, Nodes: p.Nodes, Excluded: p.Excluded, Duplicates: p.Duplicates, Invalid: p.Invalid,
				FetchedAt: p.FetchedAt, FetchErrors: p.FetchErrors, LastError: p.LastError}
			if p.Info.Known() {
				pj.Info = p.Info
			}
			out.Providers = append(out.Providers, pj)
		}
		sort.Slice(out.Providers, func(i, j int) bool { return out.Providers[i].Name < out.Providers[j].Name })
		for _, nv := range sn.Nodes {
			n, r := nv.Node, nv.Result
			nj := nodeJSON{Provider: n.Provider, Group: n.Group, Node: n.Label, Name: n.Name, Entry: n.Server,
				Tested: r.Tested, Up: r.Up, Ok: r.Ok, Fail: r.Fail}
			if r.Up {
				l := ms(r.Latency)
				nj.LatencyMs = &l
			}
			if r.ConnectOK {
				c := ms(r.Connect)
				nj.ConnectMs = &c
			}
			out.Nodes = append(out.Nodes, nj)
		}
		for _, s := range sn.Speed {
			out.Speed = append(out.Speed, speedJSON{s.Node.Provider, s.Node.Group, s.Node.Label, s.Result, s.Rate * 8 / 1e6, ms(s.TTFB), s.At})
		}
		for k, r := range sn.Rounds {
			out.Rounds[k] = roundJSON{ms(r.Duration), r.At}
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
	})
}
