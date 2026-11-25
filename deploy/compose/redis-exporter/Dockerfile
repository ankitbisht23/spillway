# oliver006/redis_exporter:v1.97.0 ships without a shell or HTTP client; add a static busybox so the
# compose healthcheck can run `/busybox wget` (ADR-0006). kind runs the base image (ADR-0085).
FROM busybox:1.38.0-musl AS busybox
FROM oliver006/redis_exporter:v1.97.0
COPY --from=busybox /bin/busybox /busybox
