# grafana/loki:3.7.9 ships without a shell or HTTP client; add a static busybox so the
# compose healthcheck can run `/busybox wget` (ADR-0006).
FROM busybox:1.38.0-musl@sha256:80e38f9a1b39188b8bdb28879178edb322a4402b081a9aedda1924609decd6f6 AS busybox
FROM grafana/loki:3.7.9@sha256:0db216b630ae1f348a1584c67f3cead8b4c1fc839169a39501db664744158e28
COPY --from=busybox /bin/busybox /busybox
