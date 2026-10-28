package probe

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/0xha0/nodeprobe/internal/config"
	"github.com/0xha0/nodeprobe/internal/node"
	"github.com/0xha0/nodeprobe/internal/provider"
	"github.com/0xha0/nodeprobe/internal/store"
)

// Runner runs one config's loops: each provider's fetches, the latency rounds, the speed rounds.
// A reload stops it and starts another on the same store.
type Runner struct {
	cfg       *config.Config
	store     *store.Store
	log       *slog.Logger
	userAgent string          // for the requests through nodes
	fetchTr   *http.Transport // the subscriptions': direct, never through a node
	client    *http.Client    // on fetchTr
	busy      sync.Mutex      // rounds never overlap (a download would slow the delays beside it); held for a whole round
	closing   sync.WaitGroup  // outbounds waiting for the round that uses them to end
	now       func() time.Time
}

// NewRunner is a runner for cfg writing into st; ua is sent with every request through a node.
func NewRunner(cfg *config.Config, st *store.Store, log *slog.Logger, ua string) *Runner {
	tr := &http.Transport{Proxy: nil, TLSHandshakeTimeout: 15 * time.Second, IdleConnTimeout: 90 * time.Second}
	return &Runner{
		cfg:       cfg,
		store:     st,
		log:       log,
		userAgent: ua,
		fetchTr:   tr,
		client:    &http.Client{Timeout: time.Minute, Transport: tr},
		now:       time.Now,
	}
}

// Run runs the loops until ctx ends, then closes what it opened.
func (r *Runner) Run(ctx context.Context) {
	r.Rebuild()
	var wg sync.WaitGroup
	for i := range r.cfg.Providers {
		p := &r.cfg.Providers[i]
		wg.Go(func() { r.providerLoop(ctx, p) })
	}
	wg.Go(func() { r.latencyLoop(ctx) })
	if *r.cfg.Speed.Enabled {
		wg.Go(func() { r.speedLoop(ctx) })
	}
	wg.Wait()
	r.closing.Wait()
	r.fetchTr.CloseIdleConnections()
}

// Rebuild remakes each provider's nodes from its last good list with this config's settings (a
// reload), so tests go on before the next fetch; a provider whose source changed is dropped (its
// old list is another list). Speed state goes when speed checks are off.
func (r *Runner) Rebuild() {
	for i := range r.cfg.Providers {
		p := &r.cfg.Providers[i]
		raw, ok := r.store.Raw(p.Name)
		switch {
		case !ok:
		case raw.Source != source(p):
			closeAll(r.store.DropProvider(p.Name))
		default:
			built := node.Build(p, r.cfg.GroupsFor(p), raw.Proxies)
			if len(built.Nodes) > 0 {
				closeAll(r.store.SetNodes(p.Name, built, raw)) // the previous runner has stopped: nothing uses them
			} else {
				closeAll(built.Nodes)
			}
		}
	}
	if !*r.cfg.Speed.Enabled {
		r.store.ClearSpeed()
	}
}

func source(p *config.Provider) string {
	if p.File != "" {
		return "file:" + p.File
	}
	return p.URL
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (r *Runner) providerLoop(ctx context.Context, p *config.Provider) {
	backoff := time.Minute
	for {
		wait := p.Refresh.Duration
		if err := r.Fetch(ctx, p); err != nil {
			wait = min(backoff, wait)
			backoff = min(2*backoff, 30*time.Minute)
		} else {
			backoff = time.Minute
		}
		if !sleep(ctx, wait) {
			return
		}
	}
}

// Fetch reads provider p's list once and puts its nodes in the store; on failure the last list
// stays. A fetch cut short by ctx (a reload, a shutdown) records nothing.
func (r *Runner) Fetch(ctx context.Context, p *config.Provider) error {
	fctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	log := r.log.With("provider", p.Name)
	list, err := provider.Fetch(fctx, r.client, p)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil {
		built := node.Build(p, r.cfg.GroupsFor(p), list.Proxies)
		for _, e := range built.Invalid {
			log.Warn("node left out", "err", e)
		}
		if len(built.Nodes) > 0 {
			old := r.store.SetNodes(p.Name, built, store.Raw{Source: source(p), Proxies: list.Proxies, Info: list.Info, At: r.now()})
			r.closeAfterRound(old)
			log.Info("node list", "nodes", len(built.Nodes), "excluded", built.Excluded, "duplicates", built.Duplicates, "invalid", len(built.Invalid))
			return nil
		}
		err = errNoNodes
	}
	r.store.FetchFailed(p.Name, err)
	log.Warn("fetch failed, keeping the last list", "err", err)
	return err
}

type constErr string

func (e constErr) Error() string { return string(e) }

const errNoNodes = constErr("no usable nodes in the list")

func closeAll(ns []*node.Node) {
	for _, n := range ns {
		_ = n.Proxy.Close()
	}
}

// closeAfterRound closes replaced outbounds once the round in progress (which may still be
// testing them) has ended: every later round takes the store's new nodes.
func (r *Runner) closeAfterRound(ns []*node.Node) {
	if len(ns) == 0 {
		return
	}
	r.closing.Go(func() {
		r.busy.Lock()
		defer r.busy.Unlock()
		closeAll(ns)
	})
}

func (r *Runner) latencyLoop(ctx context.Context) {
	for ctx.Err() == nil {
		if len(r.store.Nodes()) == 0 { // no list yet
			if !sleep(ctx, 2*time.Second) {
				return
			}
			continue
		}
		start := r.now()
		r.LatencyRound(ctx)
		if !sleep(ctx, r.cfg.Latency.Interval.Duration-r.now().Sub(start)) {
			return
		}
	}
}

// resolver looks each entry up once a round, in the worker that first needs it: a connect time
// holds no DNS, and lookups run in parallel (as many as the tests).
type resolver struct {
	mu      sync.Mutex
	entries map[string]*lookup
	timeout time.Duration
}

type lookup struct {
	once sync.Once
	ip   string
}

func (rs *resolver) ip(ctx context.Context, host string) string {
	if ip := net.ParseIP(host); ip != nil {
		return host
	}
	rs.mu.Lock()
	l := rs.entries[host]
	if l == nil {
		l = &lookup{}
		rs.entries[host] = l
	}
	rs.mu.Unlock()
	l.once.Do(func() {
		lctx, cancel := context.WithTimeout(ctx, rs.timeout)
		defer cancel()
		addrs, err := net.DefaultResolver.LookupIPAddr(lctx, host)
		if err != nil || len(addrs) == 0 {
			return
		}
		l.ip = addrs[0].IP.String()
		for _, a := range addrs { // the first IPv4 if there is one: what most dial first
			if a.IP.To4() != nil {
				l.ip = a.IP.String()
				break
			}
		}
	})
	return l.ip
}

// LatencyRound tests every node once: its delay through the node and, for nodes on TCP, a TCP
// connect to its entry.
func (r *Runner) LatencyRound(ctx context.Context) {
	r.busy.Lock()
	defer r.busy.Unlock()
	start := r.now()
	lc, cc := r.cfg.Latency, r.cfg.Connect
	rs := &resolver{entries: map[string]*lookup{}, timeout: cc.Timeout.Duration}
	sem := make(chan struct{}, lc.Concurrency)
	var wg sync.WaitGroup
	for _, n := range r.store.Nodes() {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return
		}
		wg.Go(func() {
			defer func() { <-sem }()
			tctx, cancel := context.WithTimeout(ctx, lc.Timeout.Duration)
			delay, err := Latency(tctx, n.Proxy, lc.URL, r.userAgent, *lc.Unified)
			cancel()
			if err != nil {
				delay = 0
				r.log.Debug("latency failed", "provider", n.Provider, "node", n.Label, "err", err)
			} else if delay <= 0 {
				delay = time.Microsecond // passed: never recorded as 0, which means failed
			}
			var conn time.Duration
			connOK := false
			if *cc.Enabled && n.TCP && n.Port > 0 && n.Server != "" {
				if ip := rs.ip(ctx, n.Server); ip != "" {
					cctx, cancel := context.WithTimeout(ctx, cc.Timeout.Duration)
					conn, err = Connect(cctx, net.JoinHostPort(ip, strconv.Itoa(n.Port)))
					cancel()
					connOK = err == nil
				}
			}
			if ctx.Err() == nil { // a round cut short by a reload or shutdown records nothing
				r.store.RecordLatency(n, r.now(), delay, conn, connOK)
			}
		})
	}
	wg.Wait()
	if ctx.Err() == nil {
		r.store.RecordRound("latency", r.now().Sub(start), r.now())
	}
}

func (r *Runner) speedLoop(ctx context.Context) {
	if !sleep(ctx, r.cfg.Speed.First.Duration) {
		return
	}
	for ctx.Err() == nil {
		start := r.now()
		r.SpeedRound(ctx)
		if !sleep(ctx, r.cfg.Speed.Interval.Duration-r.now().Sub(start)) {
			return
		}
	}
}

// SpeedRound downloads through one node of each (provider, group), one at a time.
func (r *Runner) SpeedRound(ctx context.Context) {
	r.busy.Lock()
	defer r.busy.Unlock()
	start := r.now()
	sc := r.cfg.Speed
	for _, n := range r.store.Pick(r.now(), r.cfg.Latency.Timeout.Duration) {
		tctx, cancel := context.WithTimeout(ctx, sc.Timeout.Duration)
		res, err := Speed(tctx, n.Proxy, sc.URL, r.userAgent, int64(sc.Bytes))
		cancel()
		if ctx.Err() != nil {
			return
		}
		out := &store.SpeedResult{Node: n, Result: store.Pass, Rate: res.Rate, TTFB: res.TTFB, Bytes: res.Bytes, At: r.now()}
		switch {
		case err != nil:
			out.Result, out.Rate, out.TTFB = store.Fail, 0, 0
		case res.Rate*8 < float64(sc.MinRate):
			out.Result = store.Slow
		}
		r.store.RecordSpeed(out)
		r.log.Info("speed check", "provider", n.Provider, "group", n.Group, "node", n.Label, "result", out.Result,
			"mbps", float64(int(res.Rate*8/1e5))/10, "err", err)
	}
	r.store.RecordRound("speed", r.now().Sub(start), r.now())
}
