package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The watchdog tests start this test binary as their worker: NODEPROBE_TEST_WORKER says how it
// behaves.
func TestMain(m *testing.M) {
	if mode := os.Getenv("NODEPROBE_TEST_WORKER"); mode != "" {
		os.Exit(fakeWorker(mode))
	}
	os.Exit(m.Run())
}

func fakeWorker(mode string) int {
	hb, err := openHeartbeat(os.Getenv(workerEnv))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	go hb.run(context.Background(), 20*time.Millisecond, func() time.Duration { return time.Second })
	work := func(d time.Duration) {
		for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
			hb.progress()
		}
	}
	if os.Getenv(restartsEnv) != "0" { // the watchdog's replacement: works a while and ends
		mode = "exit"
	}
	switch mode {
	case "exit": // works a while, then ends by itself
		work(200 * time.Millisecond)
		return 7
	case "serve": // works until a SIGTERM; a SIGHUP is a reload
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGHUP)
		for {
			select {
			case s := <-sig:
				if s == syscall.SIGHUP {
					fmt.Fprintln(os.Stderr, "reloaded")
					continue
				}
				return 0
			case <-time.After(10 * time.Millisecond):
				hb.progress()
			}
		}
	case "stall": // works, then a test never returns (the round waits for it forever)
		work(200 * time.Millisecond)
		stuckTest()
	case "freeze": // works, then stops the world for good (GODEBUG=asyncpreemptoff=1: the loop can't be stopped)
		work(200 * time.Millisecond)
		go spinForever()
		time.Sleep(50 * time.Millisecond)
		runtime.GC()
	case "deaf": // works and doesn't stop on SIGTERM
		signal.Ignore(syscall.SIGTERM)
		work(time.Hour)
	}
	return 0
}

//go:noinline
func stuckTest() {
	never := make(chan struct{})
	<-never
}

//go:noinline
func spinForever() int {
	n := 0
	for {
		n++
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// watch runs a watcher over a fake worker; signals go to sigs. It returns the exit status and all
// that was logged.
func watch(t *testing.T, mode string, env []string, dumpDir string, sigs chan os.Signal) (int, string) {
	t.Helper()
	out := &syncBuffer{}
	w := &watcher{
		argv: []string{os.Args[0]},
		// a race build sleeps a second at its exit by default (atexit_sleep_ms): not a stuck worker
		env:     append(append(os.Environ(), "NODEPROBE_TEST_WORKER="+mode, "GORACE=atexit_sleep_ms=0"), env...),
		stdout:  io.Discard,
		stderr:  out,
		log:     slog.New(slog.NewTextHandler(out, nil)),
		signals: sigs,
		// the first line's timeout: longer than the test may take, so a dump within it is the worker's 1s
		timeout:   time.Minute,
		stopGrace: 500 * time.Millisecond, dumpWait: 5 * time.Second,
		check: 50 * time.Millisecond, restartIn: 50 * time.Millisecond,
		dumpDir: dumpDir, keep: 2,
	}
	done := make(chan int, 1)
	go func() { done <- w.run() }()
	select {
	case code := <-done:
		return code, out.String()
	case <-time.After(30 * time.Second):
		t.Fatalf("the watcher didn't end:\n%s", out.String())
		return 0, ""
	}
}

func TestWatchdogPassesTheExit(t *testing.T) {
	code, out := watch(t, "exit", nil, "", make(chan os.Signal))
	if code != 7 || strings.Contains(out, "stuck") {
		t.Errorf("status %d:\n%s", code, out)
	}
}

func TestWatchdogPassesSignalsOn(t *testing.T) {
	sigs := make(chan os.Signal, 2)
	go func() {
		time.Sleep(300 * time.Millisecond)
		sigs <- syscall.SIGHUP
		time.Sleep(300 * time.Millisecond)
		sigs <- syscall.SIGTERM
	}()
	code, out := watch(t, "serve", nil, "", sigs)
	if code != 0 || !strings.Contains(out, "reloaded") || strings.Contains(out, "stuck") {
		t.Errorf("status %d:\n%s", code, out)
	}
}

// a round that never ends: no progress for the worker's timeout (1s, not the first minute)
func TestWatchdogDumpsAStuckWorker(t *testing.T) {
	dir := t.TempDir()
	for _, old := range []string{"20200101T000000.000Z", "20200102T000000.000Z"} {
		if err := os.WriteFile(filepath.Join(dir, dumpPrefix+old+".txt"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	code, out := watch(t, "stall", nil, dir, make(chan os.Signal))
	if took := time.Since(start); code != 7 || took > 10*time.Second { // 7: the replacement's
		t.Fatalf("status %d after %s:\n%s", code, took, out)
	}
	for _, want := range []string{"no progress for", "SIGQUIT", "goroutine ", ".stuckTest()", "dump saved", "a new worker"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	files, _ := filepath.Glob(filepath.Join(dir, dumpPrefix+"*.txt"))
	if len(files) != 2 || strings.Contains(files[0]+files[1], "20200101") {
		t.Fatalf("kept %v (the 2 latest)", files)
	}
	saved, err := os.ReadFile(files[1])
	if err != nil || !strings.Contains(string(saved), "no progress for") || !strings.Contains(string(saved), ".stuckTest()") {
		t.Errorf("the saved dump (%v):\n%s", err, saved)
	}
}

// the whole runtime stopped (what a goroutine can't watch): the stacks still come, the spinning
// goroutine's too
func TestWatchdogDumpsAFrozenRuntime(t *testing.T) {
	code, out := watch(t, "freeze", []string{"GODEBUG=asyncpreemptoff=1"}, "", make(chan os.Signal))
	if code != 7 {
		t.Fatalf("status %d:\n%s", code, out)
	}
	for _, want := range []string{"no progress for", ".spinForever()", "stopping the world", "a new worker"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
}

// docker stop of a worker that doesn't stop: the stacks before Docker's SIGKILL
func TestWatchdogDumpsAWorkerThatWontStop(t *testing.T) {
	sigs := make(chan os.Signal, 1)
	go func() {
		time.Sleep(300 * time.Millisecond)
		sigs <- syscall.SIGTERM
	}()
	code, out := watch(t, "deaf", nil, "", sigs)
	if code != 1 || !strings.Contains(out, "didn't stop within") || !strings.Contains(out, "goroutine ") || strings.Contains(out, "a new worker") {
		t.Errorf("status %d:\n%s", code, out)
	}
}

func TestOpenHeartbeat(t *testing.T) {
	for _, bad := range []string{"", "x", "1", "-3", "99999999"} {
		if _, err := openHeartbeat(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
