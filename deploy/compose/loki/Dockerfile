# grafana/loki:3.7.9 ships without a shell or HTTP client; add a static busybox so the
# compose healthcheck can run `/busybox wget` (ADR-0006).
FROM busybox:1.38.0-musl AS busybox
FROM grafana/loki:3.7.9
COPY --from=busybox /bin/busybox /busybox
