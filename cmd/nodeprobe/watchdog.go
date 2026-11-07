package main

// The watchdog. In serve mode nodeprobe is two processes: the first (a container's PID 1) starts
// the second, the worker, with the same arguments, and watches it. A process can't watch itself:
// a stop-the-world that never ends (one goroutine that can't be stopped) halts every goroutine,
// a watchdog goroutine too.
//
// The worker writes a line on a pipe whenever its tests have moved on (probe.Runner.Progress, at
// most every few seconds): its current watchdog.timeout in seconds. When no line came for that
// long, or the worker doesn't stop within a few seconds of a SIGTERM, the watchdog sends it
// SIGQUIT: with GOTRACEBACK=crash every thread prints its goroutines' stacks, so a goroutine that
// runs on while the others are stopped shows up too. They go to the log and, with
// watchdog.dump_dir, to a file there. A worker stuck at its work is replaced by a new one at once
// (counted: nodeprobe_watchdog_restarts_total); the watchdog doesn't leave that to Docker, whose
// restart policy skips a container's next exit after any `docker kill`, a reload's SIGHUP too.
// A worker that ends by itself ends the watchdog with its status (as nodeprobe alone did).

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/0xha0/nodeprobe/internal/config"
)

const (
	workerEnv   = "NODEPROBE_WORKER"            // in the worker: the file descriptor of its end of the pipe
	restartsEnv = "NODEPROBE_WATCHDOG_RESTARTS" // in the worker: the workers before it the watchdog replaced
)

const dumpPrefix = "nodeprobe-stacks-"

// watcher runs a worker and watches it.
type watcher struct {
	argv      []string // the worker's command line, the program first
	env       []string // the worker's environment (workerEnv and GOTRACEBACK are added)
	stdout    io.Writer
	stderr    io.Writer // the worker's log; the watcher's own goes to log
	log       *slog.Logger
	signals   <-chan os.Signal // SIGTERM / SIGINT stop the worker, SIGHUP is passed on (nil: this process's)
	restartIn time.Duration    // between a stuck worker's end and the next one's start
	timeout   time.Duration    // until the worker's first line
	stopGrace time.Duration    // after a SIGTERM, before the stack dump
	dumpWait  time.Duration    // after the SIGQUIT, before a SIGKILL
	check     time.Duration    // how often the quiet is measured
	dumpDir   string           // a file per dump here ("": the log only)
	keep      int              // dump files kept in dumpDir
}

// supervise runs this program again as the worker, under a watcher.
func supervise(cfg *config.Config, log *slog.Logger) int {
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	w := &watcher{
		argv:   append([]string{exe}, os.Args[1:]...),
		env:    os.Environ(),
		stdout: os.Stdout, stderr: os.Stderr, log: log,
		timeout: cfg.Watchdog.Timeout.Duration,
		// docker stop waits 10 s before its SIGKILL: a worker that hasn't stopped after 6 is stuck,
		// and its dump takes well under a second
		stopGrace: 6 * time.Second, dumpWait: 3 * time.Second,
		check: 5 * time.Second, restartIn: time.Second,
		dumpDir: cfg.Watchdog.DumpDir, keep: 20,
	}
	return w.run()
}

// run runs workers until one ends by itself or on a signal, and returns its status.
func (w *watcher) run() int {
	sigs := w.signals
	if sigs == nil {
		c := make(chan os.Signal, 4)
		signal.Notify(c, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
		defer signal.Stop(c)
		sigs = c
	}
	for restarts := 0; ; restarts++ {
		code, stuck := w.worker(sigs, restarts)
		if !stuck {
			return code
		}
		time.Sleep(w.restartIn)
		w.log.Error("watchdog: a new worker", "restarts", restarts+1)
	}
}

// worker runs one worker to its end: its status, and whether it was stuck at its work (and should
// be replaced).
func (w *watcher) worker(sigs <-chan os.Signal, restarts int) (int, bool) {
	pr, pw, err := os.Pipe()
	if err != nil {
		w.log.Error("watchdog: pipe", "err", err)
		return 1, false
	}
	// no context: the worker is stopped by the signals below, not killed on a cancel
	cmd := exec.CommandContext(context.Background(), w.argv[0], w.argv[1:]...) //nolint:gosec // this program itself, with its own arguments
	cmd.Env = append(slices.Clone(w.env), workerEnv+"=3", restartsEnv+"="+strconv.Itoa(restarts))
	if !slices.ContainsFunc(w.env, func(e string) bool { return strings.HasPrefix(e, "GOTRACEBACK=") }) {
		cmd.Env = append(cmd.Env, "GOTRACEBACK=crash")
	}
	dump := &capture{max: 16 << 20}
	cmd.Stdout, cmd.Stderr = w.stdout, io.MultiWriter(w.stderr, dump)
	cmd.ExtraFiles = []*os.File{pw} // fd 3
	noCoreFiles()
	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		w.log.Error("watchdog: the worker didn't start", "err", err)
		return 1, false
	}
	_ = pw.Close()

	// the quiet is measured on this process's monotonic clock: no wall clock step can fake or hide it
	start := time.Now()
	var last, limit atomic.Int64 // the last line's time since start, the timeout it carried
	limit.Store(int64(w.timeout))
	go func() {
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			if secs, err := strconv.Atoi(sc.Text()); err == nil && secs > 0 {
				limit.Store(int64(time.Duration(secs) * time.Second))
				last.Store(int64(time.Since(start)))
			}
		}
		_ = pr.Close()
	}()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	w.log.Info("watchdog", "worker", cmd.Process.Pid, "timeout", w.timeout, "dump_dir", w.dumpDir)
	tick := time.NewTicker(w.check)
	defer tick.Stop()
	var stopping <-chan time.Time
	for {
		select {
		case err := <-exited:
			code := exitCode(err)
			if code != 0 {
				w.log.Warn("watchdog: the worker exited", "status", code)
			}
			return code, false
		case s := <-sigs:
			_ = cmd.Process.Signal(s)
			if s != syscall.SIGHUP && stopping == nil {
				stopping = time.After(w.stopGrace)
			}
		case <-stopping:
			w.dump(cmd, exited, dump, fmt.Sprintf("it didn't stop within %s of the signal", w.stopGrace))
			return 1, false
		case <-tick.C:
			if quiet := time.Since(start) - time.Duration(last.Load()); quiet > time.Duration(limit.Load()) {
				w.dump(cmd, exited, dump, fmt.Sprintf("no progress for %s", quiet.Round(time.Second)))
				return 1, stopping == nil // a worker that was asked to stop isn't replaced
			}
		}
	}
}

// dump asks the worker for its goroutines' stacks (SIGQUIT), keeps them, and makes sure it ends.
func (w *watcher) dump(cmd *exec.Cmd, exited <-chan error, out *capture, reason string) {
	at := time.Now()
	w.log.Error("watchdog: the worker is stuck; its goroutines' stacks follow", "reason", reason, "pid", cmd.Process.Pid)
	out.start()
	_ = cmd.Process.Signal(syscall.SIGQUIT)
	_ = cmd.Process.Signal(syscall.SIGCONT) // a stopped process takes the SIGQUIT once continued
	select {
	case err := <-exited:
		w.log.Error("watchdog: the worker has dumped its stacks and exited", "status", exitCode(err))
	case <-time.After(w.dumpWait):
		w.log.Error("watchdog: no exit after the SIGQUIT, killing it", "after", w.dumpWait)
		_ = cmd.Process.Kill()
		err := <-exited
		w.log.Error("watchdog: the worker killed", "status", exitCode(err))
	}
	w.save(at, reason, out.bytes())
}

// save writes a dump into dumpDir (when set) and keeps the latest `keep` there.
func (w *watcher) save(at time.Time, reason string, stacks []byte) {
	if w.dumpDir == "" {
		return
	}
	name := filepath.Join(w.dumpDir, dumpPrefix+at.UTC().Format("20060102T150405.000Z")+".txt")
	head := fmt.Sprintf("nodeprobe %s (core %s): the worker was stuck at %s: %s\n\n", version, core(), at.UTC().Format(time.RFC3339), reason)
	if err := os.WriteFile(name, append([]byte(head), stacks...), 0o600); err != nil {
		w.log.Error("watchdog: the dump not saved", "err", err)
		return
	}
	w.log.Error("watchdog: dump saved", "file", name)
	old, _ := filepath.Glob(filepath.Join(w.dumpDir, dumpPrefix+"*.txt"))
	slices.Sort(old) // the names sort by time
	for len(old) > w.keep {
		_ = os.Remove(old[0])
		old = old[1:]
	}
}

// capture keeps what is written once started (up to max bytes): the worker's stack dump.
type capture struct {
	mu  sync.Mutex
	on  bool
	buf []byte
	max int
}

func (c *capture) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.on {
		c.buf = append(c.buf, b[:min(len(b), c.max-len(c.buf))]...)
	}
	return len(b), nil
}

func (c *capture) start() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.on = true
}

func (c *capture) bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.buf)
}

// exitCode is the worker's status as a shell shows it (128 + the signal that ended it).
func exitCode(err error) int {
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	default:
		return 1
	}
}

// noCoreFiles: GOTRACEBACK=crash ends in SIGABRT, which would leave a core file of the whole heap
// wherever the host's settings put one.
func noCoreFiles() {
	_ = syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{})
}

// heartbeat is the worker's end of the pipe: progress marks that the tests moved on, run writes a
// line (the current watchdog.timeout, in seconds) at most every `every` when they did. A write
// that blocks holds up this goroutine only.
type heartbeat struct {
	f     *os.File
	moved atomic.Bool
}

func openHeartbeat(fd string) (*heartbeat, error) {
	n, err := strconv.ParseUint(fd, 10, 16)
	if err != nil || n < 3 {
		return nil, fmt.Errorf("%s=%q: not a file descriptor", workerEnv, fd)
	}
	return &heartbeat{f: os.NewFile(uintptr(n), "watchdog")}, nil
}

func (h *heartbeat) progress() { h.moved.Store(true) }

func (h *heartbeat) run(ctx context.Context, every time.Duration, timeout func() time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if h.moved.Swap(false) {
				if _, err := fmt.Fprintf(h.f, "%d\n", int(timeout().Seconds())); err != nil {
					return
				}
			}
		}
	}
}
