# 0012. TLS listener and certificates

- **Status:** Accepted
- **Date:** 2026-11-04
- **Requirement IDs:** GW-04, GW-28, SEC-01, SEC-09

## Context

GW-04 asks for HTTP/1.1 and HTTP/2 with TLS termination and certificates hot-reloaded from files. SEC-01 asks for TLS 1.2 or later with modern cipher suites, optional HSTS per route, and a plain-HTTP listener for dev and benchmarks that is off by default; SEC-09 for TLS keys from env or mounted files, never from a snapshot. ADR-0008 kept `SPILLWAY_GATEWAY_HTTP_ADDR` required until a TLS listener existed. None of these says:

- which settings open which listener, and what happens with none;
- which cipher suites count as modern;
- how a reload notices new files, or what a broken pair does;
- where a dev certificate comes from on a clean clone or in CI, where `mkcert -install` can't run (it asks for a password);
- which responses carry HSTS.

## Decision

- **Settings**, from the environment like ADR-0008's:

  | Variable | Meaning | Default |
  | --- | --- | --- |
  | `SPILLWAY_GATEWAY_TLS_CERT` | PEM certificate chain file | none: TLS off |
  | `SPILLWAY_GATEWAY_TLS_KEY` | PEM private key file (SEC-09) | none; set together with the certificate |
  | `SPILLWAY_GATEWAY_TLS_ADDR` | TLS listener | `:8443` |
  | `SPILLWAY_GATEWAY_HTTP_ADDR` | plain-HTTP listener, dev and benchmarks only (SEC-01) | none: off |

  The TLS listener opens when the certificate and key are set. With neither listener configured the gateway refuses to start, so a gateway nobody configured never serves plain HTTP. One `http.Server` serves both listeners, so ADR-0010's shutdown covers both.
- **TLS** (SEC-01). TLS 1.2 is the minimum. On TLS 1.2 only ECDHE key exchange with AEAD ciphers is offered: ECDHE-ECDSA and ECDHE-RSA with AES-128-GCM, AES-256-GCM and ChaCha20-Poly1305. Go's own TLS 1.2 list would add CBC-mode suites. TLS 1.3 suites are all AEAD, and Go does not make them configurable. Key exchange groups are Go's defaults; there are no client certificates.
- **HTTP/2** (GW-04). ALPN offers `h2`, then `http/1.1`. Go's HTTP/2 server runs with its defaults, such as 250 concurrent streams per connection. GW-28's limits carry over: the TLS handshake gets the read-header timeout (10 s), read and write timeouts apply per stream, the idle timeout per connection, and the 64 KiB header limit becomes the advertised header list size. The plain-HTTP listener speaks HTTP/1.1 only (no h2c), and upstreams stay HTTP/1.1 (ADR-0008).
- **Reload** (GW-04). Both files are read at start, and a pair that does not load stops the gateway. After that they are polled every second and compared by SHA-256 of their content, like the config file; a changed pair is served to new handshakes through `GetCertificate`. A pair that fails to load (a missing file, bad PEM, a key that does not match) is logged once at error level and the current certificate stays. Logs name the certificate file, serial, DNS names and expiry, never the key.
- **`make certs`.** mkcert v1.4.4 is installed into `./bin` with `go install`, like the other tools. It mints `deploy/certs/gateway.pem` and `deploy/certs/gateway-key.pem` (gitignored) for `localhost`, `127.0.0.1`, `::1` and `gateway-1` … `gateway-5`, once; `make dev` depends on it. mkcert creates its CA in its default directory (`mkcert -CAROOT`) on first use. `mkcert -install` (setup.md) only adds that CA to the trust stores, so minting works without it. The key file is mode 0644.
- **Compose.** Every gateway serves TLS on 8443 and plain HTTP on 8000; `gateway-1` publishes both on `127.0.0.1` (docs/ports.md). `deploy/certs` is mounted read-only at `/etc/spillway-certs`: the directory, not the files, for ADR-0010's reason. Benchmarks keep using the plain-HTTP listener, and TLS runs are labelled as such (Benchmark method).
- **HSTS** (SEC-01). The route field `hsts_max_age` is a Go duration in whole seconds, 0 or more (`0s` tells browsers to drop the policy). On a TLS request, the route's upstream responses get `Strict-Transport-Security: max-age=<seconds>`, replacing any value from the upstream or a response transform. Over plain HTTP the gateway adds nothing, since RFC 6797 §7.2 forbids sending it there; an upstream's own header passes through untouched. The gateway's own answers (404, 413, 431, 502–504) carry no HSTS, as they carry no response transforms (ADR-0011).

## Consequences

- A deployment needs only the certificate and key settings; plain HTTP is something you ask for. A Kubernetes Secret mounted as a directory (M7) works as is, because polling follows its symlink swap.
- A rotation that writes the two files one after the other can log one failed load before the second file lands; the next poll serves the new pair.
- Connections already open keep the certificate they started with until they close, up to the 120 s idle timeout (GW-28) for an idle one and longer for a busy HTTP/2 connection.
- The dev key is readable by every user on the machine. On Linux a bind mount keeps the host's owner and mode, and the gateway runs as uid 65532, so a 0600 key owned by the developer could not be read; Docker Desktop on macOS maps ownership and would not need it. The key is trusted only where `mkcert -install` ran, and only for local names. A real key is never handled this way.
- To mint a new dev certificate, delete the two files, not the directory: the containers mount the directory, and on Linux a deleted and recreated directory is not the one they see. Running gateways load the new pair within a second.
- `X-Forwarded-Proto` is `https` for requests on the TLS listener (ADR-0008).
- `includeSubDomains` and `preload` are not offered. They cover every subdomain of a host, which one route can't vouch for; an additive field can add them if a requirement asks.

## Alternatives considered

- **TLS listener always on, certificate required** — every test and benchmark setup would need a certificate, even one that only uses plain HTTP; requiring at least one listener already keeps plain HTTP opt-in.
- **Go's default TLS 1.2 suites** — they include CBC-mode suites, which are not modern.
- **Watching the files with fsnotify** — misses symlink swaps unless the directory is watched too; polling matches the config watcher.
- **Restarting the listener on a new certificate** — drops in-flight requests.
- **A committed dev certificate, or one baked into the image** — test keys and certs are never committed (Testing), and an image would carry the key everywhere.
- **mkcert from Homebrew** — unpinned, and missing in CI.
- **A 0600 key, with the container running as the host user** — the host uid differs per machine and can be root, which would undo the non-root image (SEC-10).
- **HSTS on the gateway's own answers too** — response headers would then follow two rules; every route-level response header follows ADR-0011's.
