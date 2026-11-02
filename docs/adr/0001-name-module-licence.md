# 0001. Project name, Go module path and licence

- **Status:** Accepted
- **Date:** 2026-11-02
- **Requirement IDs:** none

## Context

The name becomes the repository, the Go module path, the image names, the Helm chart, the kind cluster and the Keycloak realm, so it must be fixed before the first commit: a Go module path appears in every import, and changing it later touches every file. The repository also has to be public or private before CI exists; public repositories get unlimited free minutes on GitHub's standard hosted runners (x64 and arm64), which the nightly benchmarks depend on.

## Decision

| Item | Value |
| --- | --- |
| Name | `spillway` |
| Repository | `github.com/ankitbisht23/spillway`, public |
| Go module | `github.com/ankitbisht23/spillway` |
| Licence | MIT, `Copyright (c) 2026 Ankit Bisht` |
| Images | `ghcr.io/ankitbisht23/spillway-gateway`, `ghcr.io/ankitbisht23/spillway-controlplane` |
| Helm chart | `spillway`, at `deploy/helm/spillway` |
| kind | cluster `spillway`, context `kind-spillway` |
| Keycloak realm | `spillway` |
| Kubernetes secret | `spillway-secrets` |

Every derived name uses `spillway`, so a search for the name finds every place that depends on it.

## Consequences

- The module path is tied to the GitHub account; moving the repository means rewriting every import (GitHub's redirect keeps old clones working, not the module path).
- MIT carries no explicit patent grant. Acceptable for a personal project with no patents involved.
- Being public, everything committed is published: secrets stay out of the repository (see docs/setup.md Step 8).

## Alternatives considered

- **Vanity import path** (`<domain>/spillway`) — the domain must be renewed and serve the `go-import` meta tag forever; letting it lapse or changing it later breaks every import.
- **Apache-2.0** — explicit patent grant and the usual licence for infrastructure projects; MIT chosen for its short text and no NOTICE-file handling.
- **Other names**: `ratewall`, `tollgate`, `throttle-gw`. Spillway won on the metaphor: a spillway releases excess flow in a controlled way so the dam holds, which is what rate limiting and load shedding do.
