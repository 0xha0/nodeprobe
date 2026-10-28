// Package config loads and checks nodeprobe's YAML file.
//
// ${NAME} in a value is taken from the environment, so a subscription's key can live in an env file
// next to a config that holds no secret. Only values are expanded (not comments, not keys), and an
// environment value can't change the YAML's structure. A ${NAME} that is not set is an error,
// never an empty string. $${ is a literal ${ (a regexp's named group in node_name.template). A
// value that is exactly ${NAME} takes the type of what NAME holds (`concurrency: ${C}`).
//
// In durations, sizes, rates and counts, 0 (or leaving the key out) means the default.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Config is the whole file.
type Config struct {
	Vantage   string     `yaml:"vantage"`
	Listen    string     `yaml:"listen"`
	Providers []Provider `yaml:"providers"`
	Groups    []Group    `yaml:"groups"`
	Latency   Latency    `yaml:"latency"`
	Connect   Connect    `yaml:"connect"`
	Speed     Speed      `yaml:"speed"`
}

// Provider is one node list.
type Provider struct {
	Name      string   `yaml:"name"`
	URL       string   `yaml:"url"`  // a subscription (Clash YAML, or share links / base64)
	File      string   `yaml:"file"` // or a local file of the same
	UserAgent string   `yaml:"user_agent"`
	Refresh   Duration `yaml:"refresh"`
	Include   Regexp   `yaml:"include"`    // names to keep (empty: all)
	Exclude   Regexp   `yaml:"exclude"`    // names to drop (info entries such as "expires …")
	KeepLocal bool     `yaml:"keep_local"` // nodes on this host are real (default: info entries)
	NodeName  NodeName `yaml:"node_name"`
	Groups    []Group  `yaml:"groups"` // this provider's own groups instead of the global ones
}

// NodeName turns a node's full name into the short `node` label: "country" (the default: the
// country code of the name's flag and the node's number, e.g. "HK 01"), "full" (the name as it is),
// or a mapping {pattern, template} (regexp.Expand of the pattern's match).
type NodeName struct {
	Mode     string // NodeCountry, NodeFull or NodePattern
	Pattern  Regexp
	Template string // regexp.Expand template, e.g. "$1 $2"
}

// The node_name modes.
const (
	NodeCountry = "country"
	NodeFull    = "full"
	NodePattern = "pattern"
)

// UnmarshalYAML reads "country", "full" or {pattern, template}.
func (n *NodeName) UnmarshalYAML(v *yaml.Node) error {
	if v.Kind == yaml.ScalarNode {
		if v.Value != NodeCountry && v.Value != NodeFull {
			return fmt.Errorf("line %d: node_name: country, full or {pattern, template}, not %q", v.Line, short(v.Value))
		}
		n.Mode = v.Value
		return nil
	}
	if v.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: node_name: country, full or {pattern, template}", v.Line)
	}
	for i := 0; i < len(v.Content); i += 2 {
		if k := v.Content[i].Value; k != "pattern" && k != "template" {
			return fmt.Errorf("line %d: node_name: unknown key %q", v.Content[i].Line, short(k))
		}
	}
	var m struct {
		Pattern  Regexp `yaml:"pattern"`
		Template string `yaml:"template"`
	}
	if err := v.Decode(&m); err != nil {
		return err
	}
	n.Mode, n.Pattern, n.Template = NodePattern, m.Pattern, m.Template
	return nil
}

// Group is a classification rule: the first group whose regexp matches a node's name holds it.
type Group struct {
	Name  string `yaml:"name"`
	Match Regexp `yaml:"match"`
}

// Latency is the per-node delay test.
type Latency struct {
	Interval    Duration `yaml:"interval"`
	URL         string   `yaml:"url"`
	Timeout     Duration `yaml:"timeout"`
	Concurrency int      `yaml:"concurrency"`
	// Unified (default true): the second request's round trip on the connection the first one
	// opened (as mihomo's unified-delay); false: the first request, connection set-up included.
	Unified *bool `yaml:"unified"`
}

// Connect is the TCP connect test to each node's server:port.
type Connect struct {
	Enabled *bool    `yaml:"enabled"` // default true
	Timeout Duration `yaml:"timeout"`
}

// Speed is the per-group download check.
type Speed struct {
	Enabled  *bool    `yaml:"enabled"` // default true
	Interval Duration `yaml:"interval"`
	First    Duration `yaml:"first"` // the first round, after the start
	URL      string   `yaml:"url"`   // {bytes} is replaced by Bytes
	Bytes    Size     `yaml:"bytes"`
	Timeout  Duration `yaml:"timeout"`
	Window   Duration `yaml:"window"`   // the latency history a group's node is picked by
	MinRate  Rate     `yaml:"min_rate"` // below: "slow"
}

// defaults fills what the file leaves out (and the zeros).
func (c *Config) defaults() {
	if c.Vantage == "" {
		c.Vantage = "default"
	}
	if c.Listen == "" {
		c.Listen = ":9101"
	}
	l := &c.Latency
	setBool(&l.Unified, true)
	setBool(&c.Connect.Enabled, true)
	setBool(&c.Speed.Enabled, true)
	setDuration(&l.Interval, 5*time.Minute)
	if l.URL == "" {
		l.URL = "https://cp.cloudflare.com/generate_204"
	}
	setDuration(&l.Timeout, 5*time.Second)
	if l.Concurrency == 0 {
		l.Concurrency = 8
	}
	setDuration(&c.Connect.Timeout, 3*time.Second)
	s := &c.Speed
	setDuration(&s.Interval, 4*time.Hour)
	setDuration(&s.First, 5*time.Minute)
	if s.URL == "" {
		s.URL = "https://speed.cloudflare.com/__down?bytes={bytes}"
	}
	if s.Bytes == 0 {
		s.Bytes = 5_000_000
	}
	setDuration(&s.Timeout, 10*time.Second)
	setDuration(&s.Window, 30*time.Minute)
	if s.MinRate == 0 {
		s.MinRate = 2_000_000
	}
	for i := range c.Providers {
		p := &c.Providers[i]
		setDuration(&p.Refresh, 12*time.Hour)
		if p.UserAgent == "" {
			p.UserAgent = "clash.meta" // what providers look for to answer with a Clash YAML
		}
		if p.NodeName.Mode == "" {
			p.NodeName.Mode = NodeCountry
		}
	}
}

func setBool(b **bool, v bool) {
	if *b == nil {
		*b = &v
	}
}

func setDuration(d *Duration, v time.Duration) {
	if d.Duration == 0 {
		d.Duration = v
	}
}

var labelValue = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,62}$`)

func (c *Config) validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if !labelValue.MatchString(c.Vantage) {
		bad("vantage %q: letters, digits, space . _ - (64 at most)", c.Vantage)
	}
	if len(c.Providers) == 0 {
		bad("providers: none")
	}
	seen := map[string]bool{}
	for i, p := range c.Providers {
		where := fmt.Sprintf("providers[%d] %q", i, p.Name)
		switch {
		case !labelValue.MatchString(p.Name):
			bad("%s: name: letters, digits, space . _ - (64 at most)", where)
		case seen[p.Name]:
			bad("%s: name used twice", where)
		}
		seen[p.Name] = true
		if (p.URL == "") == (p.File == "") {
			bad("%s: exactly one of url and file", where)
		}
		if p.URL != "" {
			if err := checkURL(p.URL); err != nil {
				bad("%s: url: %v", where, err)
			}
		}
		if p.Refresh.Duration < time.Minute {
			bad("%s: refresh below 1m", where)
		}
		if p.NodeName.Mode == NodePattern && (p.NodeName.Pattern.Regexp == nil || p.NodeName.Template == "") {
			bad("%s: node_name needs both a pattern and a template", where)
		}
		for _, err := range checkGroups(p.Groups) {
			bad("%s: %v", where, err)
		}
		if len(p.Groups) == 0 && len(c.Groups) == 0 {
			bad("%s: no groups (neither its own nor global ones)", where)
		}
	}
	for _, err := range checkGroups(c.Groups) {
		bad("groups: %v", err)
	}
	l := c.Latency
	if err := checkURL(l.URL); err != nil {
		bad("latency.url: %v", err)
	}
	if l.Interval.Duration < 10*time.Second {
		bad("latency.interval below 10s")
	}
	if l.Timeout.Duration <= 0 || l.Timeout.Duration >= l.Interval.Duration {
		bad("latency.timeout must be above 0 and below the interval")
	}
	if l.Concurrency < 1 || l.Concurrency > 64 {
		bad("latency.concurrency: 1 to 64")
	}
	if *c.Connect.Enabled && (c.Connect.Timeout.Duration <= 0 || c.Connect.Timeout.Duration >= l.Interval.Duration) {
		bad("connect.timeout must be above 0 and below latency.interval")
	}
	s := c.Speed
	if *s.Enabled {
		if err := checkURL(strings.ReplaceAll(s.URL, "{bytes}", "1")); err != nil {
			bad("speed.url: %v", err)
		}
		if s.Interval.Duration < time.Minute {
			bad("speed.interval below 1m")
		}
		if s.Timeout.Duration <= 0 || s.Timeout.Duration >= s.Interval.Duration {
			bad("speed.timeout must be above 0 and below the interval")
		}
		if s.Bytes <= 0 {
			bad("speed.bytes must be above 0")
		}
		if s.Window.Duration < l.Interval.Duration {
			bad("speed.window below latency.interval: it would hold no latency test")
		}
		if s.Window.Duration > 24*time.Hour {
			bad("speed.window above 24h (it is kept in memory per node)")
		}
		if s.First.Duration < 0 {
			bad("speed.first is negative")
		}
	}
	return errors.Join(errs...)
}

func checkGroups(groups []Group) []error {
	var errs []error
	seen := map[string]bool{}
	for i, g := range groups {
		switch {
		case !labelValue.MatchString(g.Name):
			errs = append(errs, fmt.Errorf("groups[%d] %q: name: letters, digits, space . _ - (64 at most)", i, g.Name))
		case seen[g.Name]:
			errs = append(errs, fmt.Errorf("groups[%d] %q: name used twice", i, g.Name))
		case g.Name == "Ungrouped":
			errs = append(errs, fmt.Errorf("groups[%d]: Ungrouped is the name of the nodes no group matches", i))
		}
		seen[g.Name] = true
		if g.Match.Regexp == nil {
			errs = append(errs, fmt.Errorf("groups[%d] %q: match is empty", i, g.Name))
		}
	}
	return errs
}

func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("not a URL") // url.Parse's message repeats the URL: it may hold a key
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme %q: http or https only", u.Scheme)
	}
	if u.Hostname() == "" {
		return errors.New("no host")
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return errors.New("bad port")
		}
	}
	return nil
}

var envRef = regexp.MustCompile(`\$?\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expand replaces each ${NAME} in the document's scalar values with the environment's value; an
// unset NAME is an error.
func expand(root *yaml.Node, lookup func(string) (string, bool)) error {
	var missing []string
	var walk func(n *yaml.Node, isKey bool)
	walk = func(n *yaml.Node, isKey bool) {
		if n.Kind == yaml.ScalarNode && !isKey {
			whole := n.Style == 0 && envRef.FindString(n.Value) == n.Value && !strings.HasPrefix(n.Value, "$$")
			n.Value = envRef.ReplaceAllStringFunc(n.Value, func(m string) string {
				if strings.HasPrefix(m, "$$") {
					return m[1:] // escaped: a literal ${…}
				}
				name := envRef.FindStringSubmatch(m)[1]
				v, ok := lookup(name)
				if !ok {
					missing = append(missing, name)
					return m
				}
				return v
			})
			if whole {
				n.Tag = "" // resolved again from the value: a number, a boolean, a string
			}
		}
		for i, c := range n.Content {
			walk(c, n.Kind == yaml.MappingNode && i%2 == 0)
		}
	}
	walk(root, false)
	if len(missing) > 0 {
		return fmt.Errorf("not set in the environment: %s", strings.Join(missing, ", "))
	}
	return nil
}

// Load reads, expands, parses, completes and checks the file at path.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b, os.LookupEnv)
}

// Parse is Load on bytes, with the environment given.
func Parse(b []byte, lookup func(string) (string, bool)) (*Config, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(b, &root); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if root.Kind == 0 {
		return nil, errors.New("empty config")
	}
	if err := expand(&root, lookup); err != nil {
		return nil, err
	}
	b, err := yaml.Marshal(&root) // and decoded again, strictly: an unknown key is an error
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	c.defaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// GroupsFor is the group list that applies to provider p.
func (c *Config) GroupsFor(p *Provider) []Group {
	if len(p.Groups) > 0 {
		return p.Groups
	}
	return c.Groups
}

// Duration is a time.Duration written as "5m", "4h", "10s".
type Duration struct{ time.Duration }

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: duration %q: not a duration (5m, 4h, 10s)", n.Line, short(n.Value))
	}
	d.Duration = v
	return nil
}

// Regexp is a compiled regular expression written as a string.
type Regexp struct{ *regexp.Regexp }

// MustRegexp compiles s or panics (for built-in defaults).
func MustRegexp(s string) Regexp { return Regexp{regexp.MustCompile(s)} }

// UnmarshalYAML implements yaml.Unmarshaler.
func (r *Regexp) UnmarshalYAML(n *yaml.Node) error {
	if n.Value == "" {
		return nil
	}
	re, err := regexp.Compile(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: regexp %q: %w", n.Line, short(n.Value), err)
	}
	r.Regexp = re
	return nil
}

// Size is a byte count written as "5MB", "512KB", "1048576" (decimal units, KiB/MiB binary).
type Size int64

var sizeUnits = map[string]int64{"": 1, "B": 1, "KB": 1e3, "MB": 1e6, "GB": 1e9, "KIB": 1 << 10, "MIB": 1 << 20, "GIB": 1 << 30}

// UnmarshalYAML implements yaml.Unmarshaler.
func (s *Size) UnmarshalYAML(n *yaml.Node) error {
	v, err := parseUnit(n.Value, sizeUnits)
	if err != nil {
		return fmt.Errorf("line %d: size %q: %w", n.Line, short(n.Value), err)
	}
	*s = Size(v)
	return nil
}

// Rate is bits per second written as "2Mbps", "500kbps", "1000000".
type Rate int64

var rateUnits = map[string]int64{"": 1, "BPS": 1, "KBPS": 1e3, "MBPS": 1e6, "GBPS": 1e9}

// UnmarshalYAML implements yaml.Unmarshaler.
func (r *Rate) UnmarshalYAML(n *yaml.Node) error {
	v, err := parseUnit(n.Value, rateUnits)
	if err != nil {
		return fmt.Errorf("line %d: rate %q: %w", n.Line, short(n.Value), err)
	}
	*r = Rate(v)
	return nil
}

// short is a value cut for an error message (a key pasted into the wrong field stays out of logs).
func short(v string) string {
	if r := []rune(v); len(r) > 24 {
		return string(r[:12]) + "…"
	}
	return v
}

var numUnit = regexp.MustCompile(`^\s*([0-9]+(?:\.[0-9]+)?)\s*([A-Za-z]*)\s*$`)

func parseUnit(s string, units map[string]int64) (int64, error) {
	m := numUnit.FindStringSubmatch(s)
	if m == nil {
		return 0, errors.New("a number with an optional unit")
	}
	mul, ok := units[strings.ToUpper(m[2])]
	if !ok {
		return 0, fmt.Errorf("unknown unit %q", m[2])
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, err
	}
	v := f * float64(mul)
	if v <= 0 || v > 1<<53 {
		return 0, errors.New("out of range")
	}
	return int64(v), nil
}
