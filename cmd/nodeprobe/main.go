// nodeprobe: tests proxy nodes from one or more providers through the mihomo (Clash Meta) core and
// reports on /metrics (Prometheus), /api/status (JSON) and /healthz.
//
//	nodeprobe -config nodeprobe.yaml          serve; SIGHUP reloads the config
//	nodeprobe -config nodeprobe.yaml -check   check the config and exit
//	nodeprobe -config nodeprobe.yaml -once    fetch, one latency round (and -speed: one speed
//	                                          round), print the status JSON, exit
//	nodeprobe -config nodeprobe.yaml -healthcheck
//	                                          exit 0 when this host's /healthz (the config's
//	                                          listen port) answers 200: a health check without curl
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	mlog "github.com/metacubex/mihomo/log"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/0xha0/nodeprobe/internal/config"
	"github.com/0xha0/nodeprobe/internal/metrics"
	"github.com/0xha0/nodeprobe/internal/probe"
	"github.com/0xha0/nodeprobe/internal/store"
)

var version = "dev"

func main() {
	path := flag.String("config", "/etc/nodeprobe/nodeprobe.yaml", "the config file")
	check := flag.Bool("check", false, "check the config and exit")
	once := flag.Bool("once", false, "one round, print the status, exit")
	speed := flag.Bool("speed", false, "with -once: a speed round too")
	level := flag.String("log-level", "info", "debug, info, warn or error")
	showVersion := flag.Bool("version", false, "print the version and exit")
	health := flag.Bool("healthcheck", false, "check the local /healthz (the config's listen port) and exit")
	healthURL := flag.String("healthcheck-url", "", "with -healthcheck: this URL instead")
	flag.Parse()

	if *health {
		target := *healthURL
		if target == "" {
			target = localHealthz(*path)
		}
		os.Exit(healthcheck(target))
	}

	if *showVersion {
		fmt.Println("nodeprobe", version, "core", core())
		return
	}
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(*level)); err != nil {
		fmt.Fprintln(os.Stderr, "log-level:", err)
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
	mlog.SetLevel(mlog.SILENT) // the core's own logging: our tests report what matters

	cfg, err := config.Load(*path)
	if err != nil {
		log.Error("config", "path", *path, "err", err)
		os.Exit(2)
	}
	if *check {
		fmt.Printf("%s: %d providers, %d groups; ok\n", *path, len(cfg.Providers), len(cfg.Groups))
		return
	}
	if *once {
		os.Exit(runOnce(cfg, log, *speed))
	}
	os.Exit(serve(*path, cfg, log))
}

// localHealthz is http://127.0.0.1:<the config's listen port>/healthz (9101 when the config can't
// be read: the default).
func localHealthz(path string) string {
	port := "9101"
	if cfg, err := config.Load(path); err == nil {
		if _, p, err := net.SplitHostPort(cfg.Listen); err == nil && p != "" {
			port = p
		}
	}
	return "http://" + net.JoinHostPort("127.0.0.1", port) + "/healthz"
}

func healthcheck(target string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthz:", resp.Status)
		return 1
	}
	return 0
}

func userAgent() string { return "nodeprobe/" + version }

// core is the mihomo module version this binary was built with.
func core() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, d := range bi.Deps {
			if d.Path == "github.com/metacubex/mihomo" {
				return d.Version
			}
		}
	}
	return "unknown"
}

func runOnce(cfg *config.Config, log *slog.Logger, speed bool) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	st := store.New(cfg.Speed.Window.Duration)
	r := probe.NewRunner(cfg, st, log, userAgent())
	ok := false
	for i := range cfg.Providers {
		if r.Fetch(ctx, &cfg.Providers[i]) == nil {
			ok = true
		}
	}
	if !ok {
		return 1
	}
	r.LatencyRound(ctx)
	if speed {
		r.SpeedRound(ctx)
	}
	metrics.Status(st, func() string { return cfg.Vantage }).ServeHTTP(stdoutWriter{}, nil)
	return 0
}

type stdoutWriter struct{}

func (stdoutWriter) Header() http.Header         { return http.Header{} }
func (stdoutWriter) Write(b []byte) (int, error) { return os.Stdout.Write(b) }
func (stdoutWriter) WriteHeader(int)             {}

func serve(path string, cfg *config.Config, log *slog.Logger) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var current atomic.Pointer[config.Config]
	current.Store(cfg)
	vantage := func() string { return current.Load().Vantage }
	st := store.New(cfg.Speed.Window.Duration)
	started := time.Now()

	reg := prometheus.NewRegistry()
	reg.MustRegister(&metrics.Collector{Store: st, Vantage: vantage, Version: version, Core: core()},
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	mux := http.NewServeMux()
	// one bad series must not cost the others: ContinueOnError serves what could be gathered
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		ErrorHandling: promhttp.ContinueOnError, ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}))
	mux.Handle("GET /api/status", metrics.Status(st, vantage))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if err := healthy(st.Snapshot(), current.Load(), started, time.Now()); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	srv := &http.Server{Addr: cfg.Listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.ListenAndServe() }()
	log.Info("nodeprobe", "version", version, "core", core(), "listen", cfg.Listen, "vantage", cfg.Vantage, "providers", len(cfg.Providers))

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	for {
		rctx, cancel := context.WithCancel(ctx)
		var wg sync.WaitGroup
		wg.Go(func() { probe.NewRunner(current.Load(), st, log, userAgent()).Run(rctx) })
		next := waitReload(ctx, path, current.Load(), hup, srvErr, log)
		cancel()
		wg.Wait()
		switch {
		case next.err != nil:
			log.Error("http server", "err", next.err)
			return 1
		case next.cfg == nil:
			shutdown(srv, log)
			return 0
		}
		keep := map[string]bool{}
		for _, p := range next.cfg.Providers {
			keep[p.Name] = true
		}
		for _, p := range st.Providers() {
			if !keep[p] {
				for _, n := range st.DropProvider(p) {
					_ = n.Proxy.Close()
				}
			}
		}
		st.SetWindow(next.cfg.Speed.Window.Duration)
		current.Store(next.cfg)
		log.Info("config reloaded", "providers", len(next.cfg.Providers))
	}
}

type reload struct {
	cfg *config.Config // nil: stop
	err error          // the HTTP server failed
}

// waitReload waits for a SIGHUP with a config that loads (a broken one is logged and the running
// one stays), a shutdown, or the HTTP server's end.
func waitReload(ctx context.Context, path string, cur *config.Config, hup <-chan os.Signal, srvErr <-chan error, log *slog.Logger) reload {
	for {
		select {
		case <-ctx.Done():
			return reload{}
		case err := <-srvErr:
			if errors.Is(err, http.ErrServerClosed) {
				return reload{}
			}
			return reload{err: err}
		case <-hup:
			next, err := config.Load(path)
			if err != nil {
				log.Error("reload refused, the running config stays", "err", err)
				continue
			}
			if next.Listen != cur.Listen {
				log.Warn("listen changes only with a restart", "listen", cur.Listen)
				next.Listen = cur.Listen
			}
			return reload{cfg: next}
		}
	}
}

func shutdown(srv *http.Server, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Warn("http shutdown", "err", err)
	}
}

// healthy: a latency round ended within three intervals (or the start was less than that ago).
func healthy(sn *store.Snapshot, cfg *config.Config, started, now time.Time) error {
	limit := 3 * cfg.Latency.Interval.Duration
	r, ok := sn.Rounds["latency"]
	switch {
	case ok && now.Sub(r.At) <= limit:
		return nil
	case !ok && now.Sub(started) <= limit:
		return nil
	case !ok:
		return errors.New("no latency round yet")
	default:
		return fmt.Errorf("last latency round %s ago", now.Sub(r.At).Round(time.Second))
	}
}
