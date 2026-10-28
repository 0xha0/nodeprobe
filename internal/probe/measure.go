// Package probe runs the tests through each node's mihomo outbound and the loops that schedule
// them.
package probe

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	C "github.com/metacubex/mihomo/constant"
)

// dialer makes Go's HTTP transport dial through a mihomo outbound.
func dialer(p C.Proxy) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, _, addr string) (net.Conn, error) {
		var md C.Metadata
		if err := md.SetRemoteAddress(addr); err != nil {
			return nil, err
		}
		return p.DialContext(ctx, &md)
	}
}

// onceDialer hands out one connection, then refuses: the second request of a unified test must
// reuse it, not open another.
type onceDialer struct {
	mu   sync.Mutex
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
	used bool
}

func (o *onceDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.used {
		return nil, errors.New("connection not reused")
	}
	o.used = true
	return o.dial(ctx, network, addr)
}

func transport(dial func(ctx context.Context, network, addr string) (net.Conn, error)) *http.Transport {
	return &http.Transport{
		DialContext:         dial,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConnsPerHost: 1,
		ForceAttemptHTTP2:   false,
		DisableCompression:  true,
	}
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// Latency is the time of one GET of target through p, within ctx. With unified it is the second GET
// on the connection the first opened (the round trip, as mihomo's unified-delay); if that second
// one fails the first's time stands, as in mihomo. Any status below 400 passes.
// ua is the User-Agent (speed.cloudflare.com answers 403 to Go's and Python's default agents).
func Latency(ctx context.Context, p C.Proxy, target, ua string, unified bool) (time.Duration, error) {
	t := transport((&onceDialer{dial: dialer(p)}).DialContext)
	defer t.CloseIdleConnections()
	client := &http.Client{Transport: t, CheckRedirect: noRedirect}
	get := func() (time.Duration, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
		if err != nil {
			return 0, err
		}
		req.Header.Set("User-Agent", ua)
		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			return 0, cleanErr(err)
		}
		d := time.Since(start)
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) // drained: the connection can be reused
		_ = resp.Body.Close()
		if resp.StatusCode >= 400 {
			return 0, fmt.Errorf("http status %d", resp.StatusCode)
		}
		return d, nil
	}
	first, err := get()
	if err != nil || !unified {
		return first, err
	}
	if second, err := get(); err == nil {
		return second, nil
	}
	return first, nil
}

// SpeedResult is one download through a node.
type SpeedResult struct {
	Bytes int64         // all that arrived
	Rate  float64       // bytes per second after the first read: what came next over the time it took
	TTFB  time.Duration // the request to the first read
}

// Speed downloads target ({bytes}: size) through p until size bytes arrived or ctx ends. Bytes
// that arrived before ctx's deadline still make a result; nothing arriving is an error. When it all
// came in one read the rate is over the whole request instead.
func Speed(ctx context.Context, p C.Proxy, target, ua string, size int64) (SpeedResult, error) {
	t := transport(dialer(p))
	defer t.CloseIdleConnections()
	client := &http.Client{Transport: t, CheckRedirect: noRedirect}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.ReplaceAll(target, "{bytes}", strconv.FormatInt(size, 10)), http.NoBody)
	if err != nil {
		return SpeedResult{}, err
	}
	req.Header.Set("User-Agent", ua)
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return SpeedResult{}, cleanErr(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return SpeedResult{}, fmt.Errorf("http status %d", resp.StatusCode)
	}
	buf := make([]byte, 64<<10)
	var got, firstBytes int64
	var first, last time.Time
	for got < size {
		n, err := resp.Body.Read(buf[:min(int64(len(buf)), size-got)])
		if n > 0 {
			if first.IsZero() {
				first, firstBytes = time.Now(), int64(n)
			}
			got += int64(n)
			last = time.Now()
		}
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				break // the end, or the time is up: what arrived counts
			}
			if got == 0 {
				return SpeedResult{}, cleanErr(err)
			}
			break
		}
	}
	if got == 0 {
		return SpeedResult{}, errors.New("no data")
	}
	r := SpeedResult{Bytes: got, TTFB: first.Sub(start)}
	if after := got - firstBytes; after > 0 && last.After(first) {
		r.Rate = float64(after) / last.Sub(first).Seconds()
	} else { // it all came in one read
		r.Rate = float64(got) / max(last.Sub(start), time.Microsecond).Seconds()
	}
	return r, nil
}

// Connect is the time of a TCP connect to addr (an IP and port), within ctx.
func Connect(ctx context.Context, addr string) (time.Duration, error) {
	var d net.Dialer
	start := time.Now()
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return 0, err
	}
	el := time.Since(start)
	_ = c.Close()
	return el, nil
}

// cleanErr keeps the cause of an HTTP client error, not the URL it repeats.
func cleanErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
