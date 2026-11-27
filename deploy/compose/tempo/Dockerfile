# grafana/tempo:3.0.0 ships without a shell or HTTP client; add a static busybox so the
# compose healthcheck can run `/busybox wget` (ADR-0006).
FROM busybox:1.38.0-musl@sha256:80e38f9a1b39188b8bdb28879178edb322a4402b081a9aedda1924609decd6f6 AS busybox
FROM grafana/tempo:3.0.0@sha256:78439f7f7cf3c97122846c13a832e060c6c7ef67dcc814dccf0a5f3f78393a93
COPY --from=busybox /bin/busybox /busybox
