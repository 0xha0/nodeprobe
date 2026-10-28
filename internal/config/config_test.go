package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

const minimal = `
providers:
  - name: main
    url: ${SUB}
groups:
  - {name: HK, match: "🇭🇰"}
  - {name: Other, match: ".*$"}
`

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte(minimal), env(map[string]string{"SUB": "https://example.com/s?key=abc"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Vantage != "default" || c.Listen != ":9101" {
		t.Errorf("vantage %q listen %q", c.Vantage, c.Listen)
	}
	if c.Latency.Interval.Duration != 5*time.Minute || c.Latency.Concurrency != 8 || !*c.Latency.Unified {
		t.Errorf("latency %+v", c.Latency)
	}
	if !*c.Connect.Enabled || !*c.Speed.Enabled || c.Speed.Bytes != 5_000_000 || c.Speed.MinRate != 2_000_000 {
		t.Errorf("connect %+v speed %+v", c.Connect, c.Speed)
	}
	p := c.Providers[0]
	if p.URL != "https://example.com/s?key=abc" || p.Refresh.Duration != 12*time.Hour || p.UserAgent != "clash.meta" {
		t.Errorf("provider %+v", p)
	}
	if got := c.Groups[1].Match.String(); got != ".*$" {
		t.Errorf("a $ in a regexp must stay: %q", got)
	}
}

func TestEnvMissing(t *testing.T) {
	_, err := Parse([]byte(minimal), env(nil))
	if err == nil || !strings.Contains(err.Error(), "SUB") {
		t.Fatalf("want an error naming SUB, got %v", err)
	}
}

func TestEnvOnlyInValues(t *testing.T) {
	y := "# ${NOT_SET} in a comment stays a comment\n" + strings.Replace(minimal, "url: ${SUB}", "url: ${SUB}\n    user_agent: ${UA}", 1)
	ua := "x'y\"z\nvantage: injected"
	c, err := Parse([]byte(y), env(map[string]string{"SUB": "https://h/s?key=a'b#c: d", "UA": ua}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Vantage != "default" || c.Providers[0].UserAgent != ua || c.Providers[0].URL != "https://h/s?key=a'b#c: d" {
		t.Errorf("an environment value changed the structure: vantage %q ua %q url %q", c.Vantage, c.Providers[0].UserAgent, c.Providers[0].URL)
	}
	if _, err := Parse([]byte(""), env(nil)); err == nil {
		t.Error("an empty config was accepted")
	}
}

func TestEnvTypesAndEscape(t *testing.T) {
	y := `
vantage: "${V}"
providers:
  - name: main
    url: ${SUB}
    node_name:
      pattern: '(?P<cc>[A-Z]{2})'
      template: '$${cc}!'
groups:
  - {name: All, match: "."}
latency:
  concurrency: ${C}
  unified: ${U}
`
	c, err := Parse([]byte(y), env(map[string]string{"SUB": "http://h/", "C": "4", "U": "false", "V": "007"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Latency.Concurrency != 4 || *c.Latency.Unified || c.Vantage != "007" {
		t.Errorf("concurrency %d unified %v vantage %q", c.Latency.Concurrency, *c.Latency.Unified, c.Vantage)
	}
	if c.Providers[0].NodeName.Template != "${cc}!" {
		t.Errorf("$${ must give a literal ${: %q", c.Providers[0].NodeName.Template)
	}
}

func TestUnits(t *testing.T) {
	c, err := Parse([]byte(minimal+`
speed: {bytes: 10MiB, min_rate: 1.5Mbps, interval: 2h, timeout: 20s, window: 1h}
latency: {interval: 1m, timeout: 3s, concurrency: 4, unified: false}
connect: {enabled: false}
`), env(map[string]string{"SUB": "http://h/x"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Speed.Bytes != 10<<20 || c.Speed.MinRate != 1_500_000 || c.Speed.Interval.Duration != 2*time.Hour {
		t.Errorf("speed %+v", c.Speed)
	}
	if *c.Latency.Unified || *c.Connect.Enabled {
		t.Errorf("explicit false lost: %+v %+v", c.Latency, c.Connect)
	}
}

func TestInvalid(t *testing.T) {
	cases := map[string]string{
		"unknown key":       minimal + "colour: red\n",
		"bad regexp":        "providers: [{name: a, url: 'http://h/'}]\ngroups: [{name: X, match: '('}]\n",
		"no providers":      "groups: [{name: X, match: x}]\n",
		"url and file":      "providers: [{name: a, url: 'http://h/', file: /x}]\ngroups: [{name: X, match: x}]\n",
		"neither":           "providers: [{name: a}]\ngroups: [{name: X, match: x}]\n",
		"duplicate name":    "providers: [{name: a, url: 'http://h/'}, {name: a, url: 'http://h/'}]\ngroups: [{name: X, match: x}]\n",
		"bad label":         "providers: [{name: 'a/b', url: 'http://h/'}]\ngroups: [{name: X, match: x}]\n",
		"no groups":         "providers: [{name: a, url: 'http://h/'}]\n",
		"empty match":       "providers: [{name: a, url: 'http://h/'}]\ngroups: [{name: X}]\n",
		"scheme":            "providers: [{name: a, url: 'ftp://h/'}]\ngroups: [{name: X, match: x}]\n",
		"timeout≥interval":  minimal + "latency: {interval: 10s, timeout: 10s}\n",
		"window<interval":   minimal + "speed: {window: 1m}\nlatency: {interval: 5m}\n",
		"bad duration":      minimal + "latency: {interval: soon}\n",
		"bad size":          minimal + "speed: {bytes: 5XB}\n",
		"concurrency":       minimal + "latency: {concurrency: 65}\n",
		"window above 24h":  minimal + "speed: {window: 25h}\n",
		"group Ungrouped":   "providers: [{name: a, url: 'http://h/'}]\ngroups: [{name: Ungrouped, match: x}]\n",
		"url port":          minimal + "latency: {url: 'http://h:99999/'}\n",
		"refresh too short": "providers: [{name: a, url: 'http://h/', refresh: 10s}]\ngroups: [{name: X, match: x}]\n",
	}
	for name, y := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(y), env(map[string]string{"SUB": "http://h/"})); err == nil {
				t.Errorf("accepted:\n%s", y)
			}
		})
	}
}

func TestURLErrorHasNoKey(t *testing.T) {
	y := "providers: [{name: a, url: 'https://h:port/s?key=SECRET'}]\ngroups: [{name: X, match: x}]\n"
	_, err := Parse([]byte(y), env(nil))
	if err == nil {
		t.Fatal("a broken URL was accepted")
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Errorf("the key is in the error: %v", err)
	}
}

func TestGroupsFor(t *testing.T) {
	c, err := Parse([]byte(`
providers:
  - {name: a, url: 'http://h/'}
  - {name: b, url: 'http://h/', groups: [{name: Mine, match: x}]}
groups: [{name: Global, match: x}]
`), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if g := c.GroupsFor(&c.Providers[0]); g[0].Name != "Global" {
		t.Errorf("a: %v", g)
	}
	if g := c.GroupsFor(&c.Providers[1]); g[0].Name != "Mine" {
		t.Errorf("b: %v", g)
	}
}
