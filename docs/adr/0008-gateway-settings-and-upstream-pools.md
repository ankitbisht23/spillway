# 0008. Gateway settings and upstream pools

- **Status:** Accepted
- **Date:** 2026-11-04
- **Requirement IDs:** GW-06, GW-21, GW-27, GW-28, GW-29, SEC-06

## Context

The gateway needs settings of its own: where the config file is, which listeners to open, which networks upstreams may be in (SEC-06), and how often to re-resolve upstream names (GW-06). If these lived in the config snapshot, the M5 admin API could change them, and the SEC-06 allow-list exists precisely so that a config change cannot point the gateway at internal or metadata addresses. GW-29 requires connection pools to survive a reload, yet a request on the old snapshot may still need an upstream that the reload removed. GW-06 asks for DNS names to be re-resolved on an interval, which Go's `http.Transport` does not do for pooled connections. GW-27 asks for container-aware `GOMAXPROCS` and `GOMEMLIMIT`, and GW-28 asks for server timeouts without saying what values.

## Decision

- **Settings come from the environment**, never from the snapshot:

  | Variable | Meaning | Default |
  | --- | --- | --- |
  | `SPILLWAY_GATEWAY_CONFIG` | YAML config file (CP-05) | required |
  | `SPILLWAY_GATEWAY_HTTP_ADDR` | plain-HTTP listener, dev and benchmarks only (SEC-01) | required until the TLS listener lands, then off by default |
  | `SPILLWAY_GATEWAY_ALLOWED_NETWORKS` | comma-separated CIDRs upstreams may be in (SEC-06) | required: no default network |
  | `SPILLWAY_GATEWAY_DNS_REFRESH` | upstream re-resolution interval (GW-06) | `30s` |

- **Allow-list** (SEC-06). `169.254.0.0/16`, `fe80::/10`, `fd00:ec2::254` and `100.100.100.200` are always rejected, even inside an allowed network. Three checks apply it:
  - At config load: an endpoint written as an IP outside the list rejects the whole snapshot, and the gateway keeps the last good one.
  - At resolution: resolved addresses outside the list are skipped and logged.
  - At connect time: `net.Dialer.Control` refuses any address outside the list.
- **Upstream registry** (GW-29). Each upstream has a pool, an `http.Transport` plus its resolved addresses, kept in a registry keyed by upstream name. The config watcher calls `Registry.Apply` on each new snapshot before swapping it in:
  - Pools of upstreams that remain are kept as they are, with their open connections.
  - Upstreams whose endpoints changed are re-resolved.
  - New upstreams get a pool.
  - Removed upstreams lose their pool and its idle connections are closed.
  
  Requests read the registry without locks. A request still on the old snapshot that names a removed upstream gets a pool rebuilt from its own snapshot, so it doesn't fail; the next reload drops that pool.
- **Transport.** It is HTTP/1.1 to upstreams (no upstream TLS or h2c in M1). It ignores `HTTP_PROXY`, has compression off so `Content-Encoding` passes through untouched, keeps 512 idle connections per address, closes idle connections after 90 s, and uses a 5 s connect timeout until per-route timeouts arrive (GW-11).
- **DNS** (GW-06). Every endpoint name is resolved when its upstream is applied, and every `SPILLWAY_GATEWAY_DNS_REFRESH` after that. Requests are sent to an IP address, so a new address takes effect on the next request rather than when old pooled connections close. If a lookup fails, the endpoint keeps its previous addresses.
- **Endpoint selection.** Round-robin across every resolved address of every endpoint, until load balancing arrives (GW-07, M4).
- **Forwarding.**
  - The upstream gets the cleaned path (ADR-0007) and the client's `Host` header.
  - The gateway sets `X-Forwarded-For`, `X-Forwarded-Host` and `X-Forwarded-Proto` itself, after removing whatever the client sent, and it removes `Forwarded` (SEC-07). Trusting `X-Forwarded-For` from known proxies is SEC-05, in M2.
  - Userinfo in an absolute-form request URL is dropped, so it never turns into an `Authorization` header.
  - `CONNECT` is refused with 405.
- **Gateway errors.**

  | Situation | Status |
  | --- | --- |
  | No route matches | 404 |
  | The upstream has no usable address | 503 |
  | The upstream request timed out | 504 |
  | Any other upstream failure | 502 |

  A connection refused by the dial-time allow-list check is logged at error level.
- **Server limits** (GW-28):

  | Limit | Value |
  | --- | --- |
  | Read-header timeout | 10 s |
  | Read timeout (whole request, body included) | 60 s |
  | Write timeout | 60 s |
  | Idle timeout | 120 s |
  | Request headers | 64 KiB; larger gets 431 |

  Per-route timeouts (GW-11) have to fit inside the write timeout.
- **Runtime** (GW-27). Since Go 1.25, `GOMAXPROCS` follows the container's CPU limit on its own. The gateway sets the Go memory limit to 90% of cgroup v2 `memory.max`, unless `GOMEMLIMIT` is set. With cgroup v1, or outside a container, no memory limit is set. Both values are logged at start.

## Consequences

- An operator can't widen the allow-list through config; changing it means a redeploy with new env.
- Reloads that keep an upstream keep its connections. `TestReloadKeepsPools` checks this: six requests across a reload use one upstream connection.
- A host name that resolves to several addresses gets all of them in rotation. Until health checks arrive (GW-08, M4), a dead address fails its share of requests. On a laptop, `localhost` resolving to both `::1` and `127.0.0.1` shows this.
- Uploads slower than 60 s and responses longer than 60 s are cut off. Streaming and long-poll routes would need the write timeout raised per route, which has no requirement yet.
- Process logs are JSON on stdout from now on; access logs and OTLP arrive with OB-03.

## Alternatives considered

- **Settings in the config snapshot** — the M5 admin API could then widen the allow-list or change the listeners.
- **Pools owned by the snapshot** — every reload would drop every pool and its connections, which GW-29 rules out.
- **Letting `http.Transport` resolve names when it dials** — pooled connections would keep using old addresses until they close, so a DNS change would take effect at an unpredictable time.
- **Writing the reverse proxy ourselves** — `httputil.ReverseProxy` already handles hop-by-hop headers, trailers, flushing and protocol upgrades correctly.
