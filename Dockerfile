# Spillway images, one target per binary (ADR-0001, ADR-0006, ADR-0014):
#   docker buildx build --target gateway --platform linux/amd64,linux/arm64 .
# Go cross-compiles on the build machine's platform, so arm64 needs no
# emulation. Final images run as non-root and work with a read-only root
# filesystem (SEC-10).

FROM --platform=$BUILDPLATFORM golang:1.27.4-trixie AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
	CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
	go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /out/ ./cmd/gateway ./cmd/loadgen ./cmd/accuracy

# distroless has no version tags, so the base is pinned by digest (ADR-0006).
FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329 AS loadgen
COPY --from=build /out/loadgen /loadgen
USER 65532:65532
ENTRYPOINT ["/loadgen"]

FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329 AS accuracy
COPY --from=build /out/accuracy /accuracy
USER 65532:65532
ENTRYPOINT ["/accuracy"]

# The last stage, so a build without --target still makes the gateway.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329 AS gateway
COPY --from=build /out/gateway /gateway
USER 65532:65532
ENTRYPOINT ["/gateway"]
