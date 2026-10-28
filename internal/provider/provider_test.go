package provider

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/0xha0/nodeprobe/internal/config"
)

const clashYAML = `
proxies:
  - {name: "🇭🇰 HK | 香港 01", type: trojan, server: a.example, port: 443, password: p}
  - {name: "🇯🇵 JP | 日本 01", type: ss, server: b.example, port: 8388, cipher: aes-128-gcm, password: p}
proxy-groups: []
`

func TestParseYAML(t *testing.T) {
	ps, err := Parse([]byte(clashYAML))
	if err != nil || len(ps) != 2 || ps[1]["server"] != "b.example" {
		t.Fatalf("%v %v", ps, err)
	}
}

func TestParseLinks(t *testing.T) {
	links := "trojan://p@a.example:443#HK%2001\nss://" + base64.RawURLEncoding.EncodeToString([]byte("aes-128-gcm:p")) + "@b.example:8388#JP%2001\n"
	for name, body := range map[string]string{"plain": links, "base64": base64.StdEncoding.EncodeToString([]byte(links))} {
		ps, err := Parse([]byte(body))
		if err != nil || len(ps) != 2 {
			t.Errorf("%s: %v %v", name, ps, err)
		}
	}
	if _, err := Parse([]byte("<html>login</html>")); err == nil {
		t.Error("a page with no nodes was accepted")
	}
}

func TestParseInfo(t *testing.T) {
	i := ParseInfo("upload=100; download=2.5e3; total=1073741824; expire=1700000000")
	if i.Upload != 100 || i.Download != 2500 || i.Total != 1<<30 || i.Expire.Unix() != 1700000000 || !i.Known() {
		t.Errorf("%+v", i)
	}
	if ParseInfo("").Known() || ParseInfo("total=x; expire=0").Known() {
		t.Error("nothing was sent, yet Known")
	}
}

func TestFetch(t *testing.T) {
	var ua string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua = r.UserAgent()
		switch r.URL.Query().Get("key") {
		case "good":
			w.Header().Set("Subscription-Userinfo", "upload=1; download=2; total=30; expire=1800000000")
			_, _ = w.Write([]byte(clashYAML))
		case "redirect":
			w.Header().Set("Location", "https://elsewhere.example/s?key=SECRETLOC%zz\x7f")
			w.WriteHeader(http.StatusFound)
		case "big":
			_, _ = w.Write([]byte(strings.Repeat("#", MaxBody+10)))
		default:
			http.Error(w, "no", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p := &config.Provider{Name: "a", URL: srv.URL + "/s?key=good", UserAgent: "clash.meta"}
	l, err := Fetch(ctx, srv.Client(), p)
	if err != nil || len(l.Proxies) != 2 || l.Info.Total != 30 || ua != "clash.meta" {
		t.Fatalf("%+v %v ua=%q", l, err, ua)
	}
	for _, key := range []string{"SECRET404", "big", "redirect"} {
		p.URL = srv.URL + "/s?key=" + key
		if _, err := Fetch(ctx, srv.Client(), p); err == nil || strings.Contains(err.Error(), "key=") || strings.Contains(err.Error(), "SECRET") {
			t.Errorf("key %s: want an error without the URL, got %v", key, err)
		}
	}
	p.URL = "http://127.0.0.1:1/s?key=SECRETDIAL" // nothing listens: a dial error
	if _, err := Fetch(ctx, srv.Client(), p); err == nil || strings.Contains(err.Error(), "SECRETDIAL") {
		t.Errorf("dial error must not carry the URL: %v", err)
	}
}
