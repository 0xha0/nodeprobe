// Package node turns a provider's proxy list into the nodes nodeprobe tests: filtered, named,
// grouped, each with its mihomo (Clash Meta) outbound.
package node

import (
	"fmt"
	"maps"
	"regexp"
	"strconv"
	"strings"
	"unicode"

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

// localServers mark an info entry: a list's "node" that only carries a line of text (traffic left,
// expiry, the provider's site) and goes nowhere, unless the provider says its nodes can be local.
// An entry without a server is one always.
var localServers = map[string]bool{"127.0.0.1": true, "0.0.0.0": true, "::1": true, "::": true, "localhost": true}

// taiwan is a name that says Taiwan; many lists flag those nodes 🇨🇳.
var taiwan = regexp.MustCompile(`(?i)台湾|臺灣|taiwan|\bTW\b`)

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
	Duplicates int     // a name seen before in the same list (tested as "<name> 2", …)
	Invalid    []error // an info entry, not a remote proxy, or mihomo could not build it
}

// Build makes provider p's nodes from its proxies, with the groups that apply to it.
func Build(p *config.Provider, groups []config.Group, proxies []map[string]any) *Built {
	b := &Built{}
	seen := map[string]bool{}
	var bases, suffixes []string // each node's name in the list and its " #2" (labels are made from the first)
	for i, m := range proxies {
		raw, _ := m["name"].(string)
		name := clean(raw)
		if name == "" {
			b.Invalid = append(b.Invalid, fmt.Errorf("proxies[%d]: no name", i))
			continue
		}
		if rest, ok := strings.CutPrefix(name, "🇨🇳"); ok && taiwan.MatchString(rest) {
			name = "🇹🇼" + rest
		}
		if rest, ok := strings.CutPrefix(name, "🇺🇲"); ok { // the US's outlying islands: lists mean the US
			name = "🇺🇸" + rest
		}
		if (p.Include.Regexp != nil && !p.Include.MatchString(name)) ||
			(p.Exclude.Regexp != nil && p.Exclude.MatchString(name)) {
			b.Excluded++
			continue
		}
		server, _ := m["server"].(string)
		if host := strings.ToLower(clean(server)); host == "" || (localServers[host] && !p.KeepLocal) {
			b.Invalid = append(b.Invalid, fmt.Errorf("%q: no real server (an info entry)", name))
			continue
		}
		base, suffix := name, ""
		if seen[name] {
			// a provider's list can name two nodes alike: both are tested, the later one numbered
			// (its short name too, from the list's name: a label would take "#2" for the node's number)
			b.Duplicates++
			for k := 2; seen[name]; k++ {
				suffix = " #" + strconv.Itoa(k)
				name = base + suffix
			}
		}
		seen[name] = true
		m = maps.Clone(m)
		m["name"] = name
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
		_, hopping := m["ports"]
		mieruUDP := typ == "mieru" && strings.EqualFold(fmt.Sprint(m["transport"]), "udp")
		b.Nodes = append(b.Nodes, &Node{
			Provider: p.Name,
			Name:     name,
			Group:    group(groups, name),
			Server:   clean(server),
			Port:     port(m["port"]),
			TCP:      tcpProtocols[typ] && !hopping && !mieruUDP,
			Proxy:    proxy,
		})
		bases, suffixes = append(bases, base), append(suffixes, suffix)
	}
	switch p.NodeName.Mode {
	case config.NodeFull:
		for _, n := range b.Nodes {
			n.Label = n.Name
		}
	case config.NodePattern:
		for i, n := range b.Nodes {
			n.Label = label(p, bases[i]) + suffixes[i]
		}
	default:
		countryLabels(b.Nodes, bases, suffixes)
	}
	uniqueLabels(b.Nodes)
	return b
}

// clean makes s valid UTF-8 (share links' names come %-unescaped and can hold any byte; Prometheus
// refuses a label value that isn't UTF-8, and one such value fails a whole scrape).
func clean(s string) string { return strings.ToValidUTF8(strings.TrimSpace(s), "�") }

// countryLabels names each node by its flag's country code and its number in the list's name
// ("🇭🇰 高级 专线 香港 01" → "HK 01"; no number: "HK"). Where a country's labels would collide
// (a list naming nodes by city, or numbering each city from 01), that country's nodes are numbered
// in list order instead. A name without a flag keeps itself.
func countryLabels(nodes []*Node, bases, suffixes []string) {
	byCode := map[string][]int{}
	var codes []string
	for i, n := range nodes {
		code := flagCode(bases[i])
		if code == "" {
			n.Label = n.Name
			continue
		}
		if byCode[code] == nil {
			codes = append(codes, code)
		}
		byCode[code] = append(byCode[code], i)
	}
	for _, code := range codes {
		idx := byCode[code]
		own, seen, clash := make([]string, len(idx)), map[string]bool{}, false
		for j, i := range idx {
			own[j] = code
			if num := nodeNumber(bases[i]); num != "" {
				own[j] += " " + num
			}
			own[j] += suffixes[i]
			clash = clash || seen[own[j]]
			seen[own[j]] = true
		}
		width := max(2, len(strconv.Itoa(len(idx))))
		for j, i := range idx {
			if clash {
				nodes[i].Label = fmt.Sprintf("%s %0*d", code, width, j+1)
			} else {
				nodes[i].Label = own[j]
			}
		}
	}
}

// flagCode is the country code of the first flag in name (two regional indicators: 🇭🇰 → "HK").
func flagCode(name string) string {
	var prev rune
	for _, r := range name {
		if r >= 0x1F1E6 && r <= 0x1F1FF {
			if prev != 0 {
				return string([]rune{prev - 0x1F1E6 + 'A', r - 0x1F1E6 + 'A'})
			}
			prev = r
			continue
		}
		prev = 0
	}
	return ""
}

// nodeNumber is the last number in name that stands on its own, two digits at least ("01"; "7" →
// "07"): one touching a letter or a dot is something else ("1x" a rate, "V1" a version, "0.2" a
// rate, "5G"); "" when there is none.
func nodeNumber(name string) string {
	rs := []rune(name)
	glued := func(i int) bool {
		return i >= 0 && i < len(rs) && (rs[i] == '.' || (rs[i] < 0x80 && unicode.IsLetter(rs[i])))
	}
	last := ""
	for i := 0; i < len(rs); {
		if !unicode.IsDigit(rs[i]) || rs[i] >= 0x80 {
			i++
			continue
		}
		j := i
		for j < len(rs) && rs[j] < 0x80 && unicode.IsDigit(rs[j]) {
			j++
		}
		if !glued(i-1) && !glued(j) {
			last = string(rs[i:j])
		}
		i = j
	}
	if len(last) == 1 {
		last = "0" + last
	}
	return last
}

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
