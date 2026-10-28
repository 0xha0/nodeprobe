# nodeprobe

Tests the nodes of one or more proxy providers through the
[mihomo](https://github.com/MetaCubeX/mihomo) (Clash Meta) core and reports on
`/metrics` for Prometheus. One static binary, one small container: the core is built in, nodes
are dialled directly, no mihomo process or listener ports to manage.

- **Providers**: a provider's own subscription link as it is, from a URL or a file: a full Clash /
  mihomo config (only its `proxies` are read; rules and groups are not needed) or a share-link list
  (plain or base64). Each is refreshed on its own interval; the last good list stays when a fetch
  fails. `subscription-userinfo` (traffic used, total, expiry) is exported when a provider sends it.
- **The nodes of a list**: info entries (a "node" carrying traffic left or an expiry date, its
  server empty, `127.0.0.1`, `0.0.0.0`, `::1` or `localhost`; `keep_local: true` when a provider's
  nodes really are on this host) are left out, as are types that are not remote proxies (`direct`,
  `dns`, `reject`, overlay networks) and `dialer-proxy` chains: all counted as invalid. Only remote
  proxies are tested (ss, ssr, vmess, vless, trojan, hysteria/2, tuic, wireguard, anytls, snell,
  socks5, http, …). Two nodes with one name are both tested, the later one as `<name> #2`. A node
  flagged 🇨🇳 whose name says Taiwan (台湾, 臺灣, Taiwan, TW) is flagged 🇹🇼, as many lists get it wrong.
- **Classification**: include / exclude by name, a short `node` label (default: country code and
  number, `🇭🇰 HK | 香港 01` → `HK 01`), groups by ordered regexps (global or per provider).
- **Latency**, every node each interval: a request through the node, as the round trip once
  connected (mihomo's unified-delay) or with the set-up; for nodes on TCP, a TCP connect to the
  entry server beside it (DNS excluded), which tells "our side cannot reach the entry" from "the
  entry cannot reach the rest" (UDP protocols and port hopping get none). Every test is counted, so
  a success rate over a window is the share of the tests in it that passed.
- **Speed** check of each (provider, group): a download through the member with the best average
  latency over a window (a failure counts as the timeout); when all of them failed, the one that
  passed last. Pass, slow (below `min_rate`) or fail: a health check, not a ranking.

## Run

```sh
docker run -d --name nodeprobe -p 9101:9101 \
  -v ./config:/etc/nodeprobe:ro --env-file nodeprobe.env \
  ghcr.io/0xha0/nodeprobe:latest
docker kill -s HUP nodeprobe      # reload the config
```

The config: [nodeprobe.example.yaml](nodeprobe.example.yaml), as `config/nodeprobe.yaml`, readable
by uid 65532 (the image's user). Mount the directory, not the file: an editor that saves a new file
leaves a single-file mount on the old one. `SIGHUP` reloads it: each provider's nodes are rebuilt
from its last list at once (a provider whose URL or file changed is fetched afresh); a config that
does not load is refused and the running one stays; `listen` changes only with a restart.

`${NAME}` in a value comes from the environment (an unset one is an error); a value that is exactly
`${NAME}` takes the type of what it holds (`concurrency: ${C}`); `$${` is a literal `${` (a named
group in `node_name.template`). In durations, sizes and counts, 0 means the default.

```sh
nodeprobe -config nodeprobe.yaml -check          # check the config
nodeprobe -config nodeprobe.yaml -once -speed    # one round of each, the status as JSON
nodeprobe -config nodeprobe.yaml -healthcheck    # 0 when the local /healthz is ok (the image's HEALTHCHECK)
```

Endpoints: `/metrics`, `/api/status` (the last results as JSON), `/healthz` (503 when no latency
round ended within three intervals).

## Metrics

Node series carry `vantage, provider, group, node, entry`.

| metric | |
|---|---|
| `nodeprobe_node_up` | 1 when the last latency test passed |
| `nodeprobe_node_last_latency_seconds` | the last latency (absent when it failed) |
| `nodeprobe_latency_seconds` | histogram of the tests that passed |
| `nodeprobe_node_checks_total{result="ok"\|"fail"}` | latency tests; both series from the start |
| `nodeprobe_node_connect_seconds` | TCP connect to the entry (absent when it failed or wasn't run) |
| `nodeprobe_speed_result` | per group, `node` = the one tested: 1 pass, 0.5 slow, 0 fail |
| `nodeprobe_speed_bytes_per_second`, `_ttfb_seconds`, `_timestamp_seconds` | the group's last check (rate after the first read) |
| `nodeprobe_speed_tests_total{result}` | per group: pass, slow, fail; all three from the start |
| `nodeprobe_speed_bytes_total` | per provider: what the checks downloaded; from the start |
| `nodeprobe_provider_nodes`, `_excluded_nodes`, `_duplicate_nodes`, `_invalid_nodes` | the last list (duplicates: tested, numbered) |
| `nodeprobe_provider_fetch_timestamp_seconds`, `_fetch_errors_total` | fetches |
| `nodeprobe_provider_traffic_used_bytes`, `_traffic_total_bytes`, `_expiry_timestamp_seconds` | from `subscription-userinfo` |
| `nodeprobe_build_info{version,core}`, `nodeprobe_round_seconds{kind}`, `nodeprobe_round_timestamp_seconds{kind}` | itself |

Success rate of a node over a day:

```promql
sum by (provider, node) (increase(nodeprobe_node_checks_total{result="ok"}[1d]))
  / sum by (provider, node) (increase(nodeprobe_node_checks_total[1d]))
```

## Notes

- A subscription URL usually carries a key: it never appears in a log line, a metric, the status
  or an error.
- `speed.cloudflare.com` refuses Go's and Python's default user agents; requests through nodes
  send `nodeprobe/<version>`.
- Latency and speed rounds never overlap: a download would slow the delays measured beside it.
  Outbounds replaced by a fetch are closed when the round still testing them ends.
- Node names are made valid UTF-8 (share links can carry any byte), and short names that collide
  fall back to full names, so no list can break a scrape.

## Build

`make` (this machine), `make test` (vet, race tests), `make lint` (golangci-lint), `make vuln`
(govulncheck), `make release` (tarballs and sha256s, GNU tar). A numeric tag (`1`, `2-rc1`) builds
the release and the image.

## License

GPL-3.0, as the mihomo core it is built with ([LICENSE](LICENSE)).
