package node

import (
	"testing"
	"unicode/utf8"

	"github.com/0xha0/nodeprobe/internal/config"
)

func proxy(name, server string) map[string]any {
	return map[string]any{"name": name, "type": "http", "server": server, "port": 8080}
}

func TestBuild(t *testing.T) {
	p := &config.Provider{
		Name:     "main",
		Exclude:  config.MustRegexp("到期"),
		NodeName: config.NodeName{Pattern: config.MustRegexp(`\b([A-Z]{2})\s*\|.*?(\d+)\s*$`), Template: "$1 $2"},
	}
	groups := []config.Group{{Name: "HK", Match: config.MustRegexp("🇭🇰")}, {Name: "EU", Match: config.MustRegexp("🇩🇪|🇬🇧")}}
	b := Build(p, groups, []map[string]any{
		proxy("🇭🇰 HK | 香港 01", "a"),
		proxy("🇭🇰 HK | 香港 01", "dup"),
		proxy("🇩🇪 DE | 德国 02", "b"),
		proxy("🇺🇸 US | 美国 03", "c"),
		proxy("到期：2027-01-01", "x"),
		proxy("plain name", "d"),
		{"name": "bad", "type": "nonsense", "server": "e", "port": 1},
		{"type": "http", "server": "f", "port": 1},
	})
	if b.Excluded != 1 || b.Duplicates != 1 || len(b.Invalid) != 2 || len(b.Nodes) != 4 {
		t.Fatalf("excluded %d duplicates %d invalid %v nodes %d", b.Excluded, b.Duplicates, b.Invalid, len(b.Nodes))
	}
	want := []struct{ label, group, server string }{{"HK 01", "HK", "a"}, {"DE 02", "EU", "b"}, {"US 03", Ungrouped, "c"}, {"plain name", Ungrouped, "d"}}
	for i, w := range want {
		n := b.Nodes[i]
		if n.Label != w.label || n.Group != w.group || n.Server != w.server || n.Port != 8080 || n.Provider != "main" || n.Proxy == nil {
			t.Errorf("node %d: %+v, want %+v", i, n, w)
		}
	}
}

func TestLabelCollision(t *testing.T) {
	p := &config.Provider{Name: "x", NodeName: config.NodeName{Pattern: config.MustRegexp(`([A-Z]{2})`), Template: "$1"}}
	b := Build(p, []config.Group{{Name: "All", Match: config.MustRegexp(".")}}, []map[string]any{
		proxy("HK one", "a"), proxy("HK two", "b"), proxy("JP one", "c"),
	})
	got := []string{b.Nodes[0].Label, b.Nodes[1].Label, b.Nodes[2].Label}
	if got[0] != "HK one" || got[1] != "HK two" || got[2] != "JP" {
		t.Errorf("colliding short names must fall back to the full ones: %v", got)
	}
}

func TestInclude(t *testing.T) {
	p := &config.Provider{Name: "x", Include: config.MustRegexp("HK"), NodeName: config.NodeName{Pattern: config.MustRegexp("^$"), Template: "$0"}}
	b := Build(p, []config.Group{{Name: "All", Match: config.MustRegexp(".")}}, []map[string]any{proxy("HK 1", "a"), proxy("JP 1", "b")})
	if len(b.Nodes) != 1 || b.Excluded != 1 || b.Nodes[0].Label != "HK 1" {
		t.Errorf("%+v", b)
	}
}

func TestTypesAndTransport(t *testing.T) {
	p := &config.Provider{Name: "x", NodeName: config.NodeName{Pattern: config.MustRegexp("^$"), Template: "$0"}}
	all := []config.Group{{Name: "All", Match: config.MustRegexp(".")}}
	b := Build(p, all, []map[string]any{
		{"name": "direct", "type": "direct"},
		{"name": "chained", "type": "http", "server": "a", "port": 1, "dialer-proxy": "other"},
		{"name": "h2", "type": "hysteria2", "server": "b", "port": 443, "password": "p"},
		{"name": "hop", "type": "hysteria2", "server": "b", "port": 443, "ports": "443-450", "password": "p"},
		{"name": "tcp", "type": "trojan", "server": "c", "port": 443, "password": "p"},
	})
	if len(b.Invalid) != 2 || len(b.Nodes) != 3 {
		t.Fatalf("direct and dialer-proxy must be left out: invalid %v nodes %d", b.Invalid, len(b.Nodes))
	}
	if b.Nodes[0].TCP || b.Nodes[1].TCP || !b.Nodes[2].TCP {
		t.Errorf("UDP protocols and port hopping get no TCP connect test: %v %v %v", b.Nodes[0].TCP, b.Nodes[1].TCP, b.Nodes[2].TCP)
	}
}

func TestInvalidUTF8(t *testing.T) {
	p := &config.Provider{Name: "x", NodeName: config.NodeName{Pattern: config.MustRegexp(`([A-Z]{2})(.*)`), Template: "$1$2"}}
	b := Build(p, []config.Group{{Name: "All", Match: config.MustRegexp(".")}}, []map[string]any{proxy("HK\xff01", "h\xfe")})
	n := b.Nodes[0]
	if !utf8.ValidString(n.Name) || !utf8.ValidString(n.Label) || !utf8.ValidString(n.Server) {
		t.Errorf("not valid UTF-8: %q %q %q", n.Name, n.Label, n.Server)
	}
}

func TestLabelCollisionChain(t *testing.T) {
	// "🇯🇵 JP 02" → "JP 02"; "JP 02" and "HK 02" → "02" collide and fall back to their full names,
	// and "JP 02" now equals the first one's label: it must fall back too
	p := &config.Provider{Name: "x", NodeName: config.NodeName{Pattern: config.MustRegexp(`^\S+ (.*)$`), Template: "$1"}}
	b := Build(p, []config.Group{{Name: "All", Match: config.MustRegexp(".")}}, []map[string]any{
		proxy("🇯🇵 JP 02", "a"), proxy("JP 02", "b"), proxy("HK 02", "c"),
	})
	seen := map[string]bool{}
	for _, n := range b.Nodes {
		if seen[n.Label] {
			t.Errorf("label %q twice", n.Label)
		}
		seen[n.Label] = true
	}
}
