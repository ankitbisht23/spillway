# mccutchen/go-httpbin:2.26.0 ships without a shell or HTTP client; add a static busybox so the
# compose healthcheck can run `/busybox wget` (ADR-0006).
FROM busybox:1.38.0-musl AS busybox
FROM mccutchen/go-httpbin:2.26.0
COPY --from=busybox /bin/busybox /busybox
