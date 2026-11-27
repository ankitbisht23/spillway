# mccutchen/go-httpbin:2.26.0 ships without a shell or HTTP client; add a static busybox so the
# compose healthcheck can run `/busybox wget` (ADR-0006).
FROM busybox:1.38.0-musl@sha256:80e38f9a1b39188b8bdb28879178edb322a4402b081a9aedda1924609decd6f6 AS busybox
FROM mccutchen/go-httpbin:2.26.0@sha256:873b4fdb27fe7cc66fa12cb9362574e1b0bf31df7ca8a71b2502988327a0b8a4
COPY --from=busybox /bin/busybox /busybox
