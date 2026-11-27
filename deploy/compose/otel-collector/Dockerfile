# otel/opentelemetry-collector-contrib:0.164.0 ships without a shell or HTTP client; add a static busybox so the
# compose healthcheck can run `/busybox wget` (ADR-0006).
FROM busybox:1.38.0-musl@sha256:80e38f9a1b39188b8bdb28879178edb322a4402b081a9aedda1924609decd6f6 AS busybox
FROM otel/opentelemetry-collector-contrib:0.164.0@sha256:340c9ca6a36617084c6661bdc4f0fe92b7cda5a805d61673ec8db375fddc522b
COPY --from=busybox /bin/busybox /busybox
