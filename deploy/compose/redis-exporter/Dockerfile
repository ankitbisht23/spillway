# oliver006/redis_exporter:v1.97.0 ships without a shell or HTTP client; add a static busybox so the
# compose healthcheck can run `/busybox wget` (ADR-0006). kind runs the base image (ADR-0085).
FROM busybox:1.38.0-musl@sha256:80e38f9a1b39188b8bdb28879178edb322a4402b081a9aedda1924609decd6f6 AS busybox
FROM oliver006/redis_exporter:v1.97.0@sha256:f5f903fbe77571d455ede0453b084d6840bc62fc105efac001034421cab83b99
COPY --from=busybox /bin/busybox /busybox
