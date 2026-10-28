// Package node turns a provider's proxy list into the nodes nodeprobe tests: filtered, named,
// grouped, each with its mihomo (Clash Meta) outbound.
package node

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/metacubex/mihomo/adapter"
	C "github.com/metacubex/mihomo/constant"

	"github.com/0xha0/nodeprobe/internal/config"
)

// Ungrouped holds the nodes no group rule matched.
const Ungrouped = "Ungrouped"

// protocols are the outbound types tested: remote proxies. mihomo also builds local or overlay
// outbounds (direct, dns, reject, rematch, openvpn, tailscale, zerotier, easytier): a list naming
// one is not a node list's business, and a direct "node" would measure our own line.
var protocols = map[string]bool{
	"ss": true, "ssr": true, "socks5": true, "http": true, "vmess": true, "vless": true, "snell": true,
	"trojan": true, "hysteria": true, "hysteria2": true, "wireguard": true, "tuic": true,
	"shadowquic": true, "gost-relay": true, "ssh": true, "mieru": true, "anytls": true, "sudoku": true,
	"masque": true, "trusttunnel": true,
}

// tcpProtocols reach their server over TCP, so a TCP connect to server:port says something.
// The others (hysteria, hysteria2, tuic, wireguard, shadowquic, masque, trusttunnel) use UDP.
var tcpProtocols = map[string]bool{
	"ss": true, "ssr": true, "socks5": true, "http": true, "vmess": true, "vless": true, "snell": true,
	"trojan": true, "gost-relay": true, "ssh": true, "mieru": true, "anytls": true, "sudoku": true,
}

// Node is one tested node.
type Node struct {
	Provider string // the provider's name
	Name     string // the node's full name in its list (valid UTF-8)
	Label    string // the short name (node_name), unique within the provider
	Group    string // the first group whose rule matched, or Ungrouped
	Server   string // the entry: the server the node's traffic goes to first
	Port     int
	TCP      bool // a TCP connect to Server:Port is a meaningful test
	Proxy    C.Proxy
}

// Key identifies a node across fetches: its provider and full name.
func (n *Node) Key() string { return n.Provider + "\x00" + n.Name }

// Built is one provider's nodes plus what was left out and why.
type Built struct {
	Nodes      []*Node
	Excluded   int     // by include / exclude
	Duplicates int     // a name seen before in the same list
	Invalid    []error // not a remote proxy, or mihomo could not build it
}

// Build makes provider p's nodes from its proxies, with the groups that apply to it.
func Build(p *config.Provider, groups []config.Group, proxies []map[string]any) *Built {
	b := &Built{}
	seen := map[string]bool{}
	for i, m := range proxies {
		raw, _ := m["name"].(string)
		name := clean(raw)
		if name == "" {
			b.Invalid = append(b.Invalid, fmt.Errorf("proxies[%d]: no name", i))
			continue
		}
		if (p.Include.Regexp != nil && !p.Include.MatchString(name)) ||
			(p.Exclude.Regexp != nil && p.Exclude.MatchString(name)) {
			b.Excluded++
			continue
		}
		if seen[name] {
			b.Duplicates++
			continue
		}
		seen[name] = true
		typ, _ := m["type"].(string)
		if !protocols[typ] {
			b.Invalid = append(b.Invalid, fmt.Errorf("%q: type %q is not a remote proxy", name, clean(typ)))
			continue
		}
		if dp, _ := m["dialer-proxy"].(string); dp != "" {
			// it would go through another outbound by name: there is no such table here
			b.Invalid = append(b.Invalid, fmt.Errorf("%q: dialer-proxy is not supported", name))
			continue
		}
		proxy, err := adapter.ParseProxy(m, adapter.WithProviderName(p.Name))
		if err != nil {
			b.Invalid = append(b.Invalid, fmt.Errorf("%q: %w", name, err))
			continue
		}
		server, _ := m["server"].(string)
		_, hopping := m["ports"]
		mieruUDP := typ == "mieru" && strings.EqualFold(fmt.Sprint(m["transport"]), "udp")
		b.Nodes = append(b.Nodes, &Node{
			Provider: p.Name,
			Name:     name,
			Label:    label(p, name),
			Group:    group(groups, name),
			Server:   clean(server),
			Port:     port(m["port"]),
			TCP:      tcpProtocols[typ] && !hopping && !mieruUDP,
			Proxy:    proxy,
		})
	}
	uniqueLabels(b.Nodes)
	return b
}

// clean makes s valid UTF-8 (share links' names come %-unescaped and can hold any byte; Prometheus
// refuses a label value that isn't UTF-8, and one such value fails a whole scrape).
func clean(s string) string { return strings.ToValidUTF8(strings.TrimSpace(s), "�") }

// label is node_name's template applied to the name's match, or the full name.
func label(p *config.Provider, name string) string {
	re := p.NodeName.Pattern.Regexp
	m := re.FindStringSubmatchIndex(name)
	if m == nil {
		return name
	}
	if l := clean(string(re.ExpandString(nil, p.NodeName.Template, name, m))); l != "" {
		return l
	}
	return name
}

// uniqueLabels gives the nodes whose short names collide their full names back, again until no two
// labels are equal (a full name can equal another node's short name). Full names are unique, so it
// ends: each pass turns at least one more label into its full name.
func uniqueLabels(nodes []*Node) {
	for {
		count := map[string]int{}
		for _, n := range nodes {
			count[n.Label]++
		}
		changed := false
		for _, n := range nodes {
			if count[n.Label] > 1 && n.Label != n.Name {
				n.Label = n.Name
				changed = true
			}
		}
		if !changed {
			return
		}
	}
}

func group(groups []config.Group, name string) string {
	for _, g := range groups {
		if g.Match.MatchString(name) {
			return g.Name
		}
	}
	return Ungrouped
}

// port reads a node's port as YAML or a share link gives it; 0 when it is not a valid port.
func port(v any) int {
	var n int64
	switch p := v.(type) {
	case int:
		n = int64(p)
	case int64:
		n = p
	case uint64:
		if p > 65535 {
			return 0
		}
		n = int64(p)
	case float64:
		n = int64(p)
	case string:
		n, _ = strconv.ParseInt(p, 10, 32)
	}
	if n < 1 || n > 65535 {
		return 0
	}
	return int(n)
}
