// Package provider fetches a provider's node list: a Clash YAML (its `proxies`), or share links
// (ss://, vmess://, trojan://, … one a line, plain or base64), from a URL or a local file.
//
// A subscription URL usually holds a key. Nothing here returns an error that contains the URL.
package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/metacubex/mihomo/common/convert"
	"go.yaml.in/yaml/v3"

	"github.com/0xha0/nodeprobe/internal/config"
)

// MaxBody is the largest node list read (a few hundred nodes are well under 1 MiB).
const MaxBody = 16 << 20

// Info is what a provider's subscription-userinfo header says (zero: not sent).
type Info struct {
	Upload   int64     `json:"upload,omitempty"`
	Download int64     `json:"download,omitempty"`
	Total    int64     `json:"total,omitempty"`
	Expire   time.Time `json:"expire,omitzero"`
}

// Known reports whether the provider sent anything.
func (i Info) Known() bool {
	return i.Total > 0 || i.Upload > 0 || i.Download > 0 || !i.Expire.IsZero()
}

// List is one fetch's result.
type List struct {
	Proxies []map[string]any
	Info    Info
}

// Fetch reads provider p's list, from its URL (with client) or its file.
func Fetch(ctx context.Context, client *http.Client, p *config.Provider) (*List, error) {
	var (
		body []byte
		info Info
		err  error
	)
	if p.File != "" {
		body, err = os.ReadFile(p.File)
		if err != nil {
			return nil, err
		}
	} else {
		body, info, err = get(ctx, client, p.URL, p.UserAgent)
		if err != nil {
			return nil, err
		}
	}
	proxies, err := Parse(body)
	if err != nil {
		return nil, err
	}
	return &List{Proxies: proxies, Info: info}, nil
}

func get(ctx context.Context, client *http.Client, rawURL, ua string) ([]byte, Info, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return nil, Info{}, errors.New("bad url")
	}
	req.Header.Set("User-Agent", ua)
	resp, err := client.Do(req)
	if err != nil {
		return nil, Info{}, redact(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, Info{}, fmt.Errorf("http status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody+1))
	if err != nil {
		return nil, Info{}, redact(err)
	}
	if len(body) > MaxBody {
		return nil, Info{}, fmt.Errorf("list larger than %d bytes", MaxBody)
	}
	return body, ParseInfo(resp.Header.Get("Subscription-Userinfo")), nil
}

// redact turns a request error into its kind. The message of a *url.Error repeats the URL (key
// included), and what it wraps can quote another (a redirect's Location): none of it is kept.
func redact(err error) error {
	var (
		dns *net.DNSError
		op  *net.OpError
		ne  net.Error
	)
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return errors.New("timeout")
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.As(err, &dns):
		return fmt.Errorf("dns: %s", dns.Err)
	case errors.As(err, &op):
		return fmt.Errorf("%s: %s", op.Op, opCause(op))
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return errors.New("connection closed early")
	case strings.Contains(err.Error(), "tls:"), strings.Contains(err.Error(), "x509:"):
		return errors.New("tls handshake failed")
	case strings.Contains(err.Error(), "redirect"):
		return errors.New("redirect failed")
	}
	return errors.New("request failed")
}

func opCause(op *net.OpError) string {
	var se *os.SyscallError
	if errors.As(op.Err, &se) {
		return se.Err.Error()
	}
	return "failed"
}

// Parse reads a Clash YAML's proxies, or else share links.
func Parse(body []byte) ([]map[string]any, error) {
	var doc struct {
		Proxies []map[string]any `yaml:"proxies"`
	}
	if err := yaml.Unmarshal(body, &doc); err == nil && len(doc.Proxies) > 0 {
		return doc.Proxies, nil
	}
	proxies, err := convert.ConvertsV2Ray(body)
	if err == nil && len(proxies) > 0 {
		return proxies, nil
	}
	return nil, errors.New("no nodes: neither a Clash YAML with proxies nor share links")
}

// ParseInfo reads "upload=1; download=2; total=3; expire=1700000000".
func ParseInfo(h string) Info {
	var i Info
	for _, part := range strings.Split(h, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		n, err := strconv.ParseFloat(strings.TrimSpace(v), 64) // some providers send 1.2e10
		if err != nil || n < 0 {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "upload":
			i.Upload = int64(n)
		case "download":
			i.Download = int64(n)
		case "total":
			i.Total = int64(n)
		case "expire":
			if n > 0 {
				i.Expire = time.Unix(int64(n), 0)
			}
		}
	}
	return i
}
