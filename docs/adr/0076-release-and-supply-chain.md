# 0076. Release v1.0.0 and the supply chain

- **Status:** Accepted
- **Date:** 2026-11-24
- **Requirement IDs:** SEC-10, CP-19, M7 release box

## Context

M7's last box asks for release v1.0.0: a changelog, an SBOM, and signed multi-arch images `ghcr.io/ankitbisht23/spillway-gateway` and `ghcr.io/ankitbisht23/spillway-controlplane`, signed with cosign keyless through GitHub OIDC. The Stack row names Syft for the SBOM, cosign keyless, Dependabot, `govulncheck`, Actions pinned to SHAs and gitleaks. M0 pinned compose images to exact tags with "digests at release". In this run the release is **prepared only**: nothing may be published outside the repository (no ghcr push, no GitHub release, no tag) without the user; the release box stays unticked.

## Decision

- **Changelog.** `CHANGELOG.md` at the root, [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/) format, with an `Unreleased` section during M7 and a `1.0.0` section written by the release task: one subsection per milestone M0–M7 listing what shipped by requirement ID, then "Known limitations" (re-baselined or open NFRs, `make verify-m3`'s laptop pass rate, kind-only Kubernetes support). The GitHub release notes are that section verbatim.
- **Workflow** `.github/workflows/release.yml`:
  - **Triggers:** `push: tags: ['v*']` and `workflow_dispatch` with a boolean input `dry_run` (default `true`). Never `push` to a branch, never `pull_request`, never `schedule`.
  - **Job `build`** (no write permissions; `contents: read`): matrix-free, on `ubuntu-latest`; QEMU and Buildx; builds the root Dockerfile's `gateway` and `controlplane` targets for `linux/amd64,linux/arm64`; runs the image smoke checks CI already runs (non-root user 65532, read-only root filesystem start); generates SBOMs; uploads the SBOMs and the OCI archives as workflow artifacts. A dry run stops here.
  - **Job `publish`** (only when the ref is a `v*` tag and not a dry run; `permissions: contents: write, packages: write, id-token: write`; a protected GitHub environment `release` that requires the user's approval): logs in to ghcr with `GITHUB_TOKEN`, pushes both images tagged `v1.0.0` and `1.0` (no `latest` floating tag in v1), signs each pushed **digest** with `cosign sign --yes` (keyless: Fulcio certificate from the workflow's OIDC token, Rekor transparency log), attaches each image's SBOM as a cosign attestation (`cosign attest --type spdxjson`), verifies both with `cosign verify` / `verify-attestation` against the identity `https://github.com/ankitbisht23/spillway/.github/workflows/release.yml@refs/tags/v1.0.0` and issuer `https://token.actions.githubusercontent.com`, and creates the GitHub release with the changelog section, the SBOMs, a source SBOM and `SHA256SUMS`.
- **SBOM.** Syft (CLI, the newest release at least two weeks old at the release task, pinned exactly, installed by its SHA-pinned action `anchore/sbom-action`'s `syft-version` input or by checksum), format **SPDX 2.3 JSON** (`spdx-json`), one per image per platform and one for the source tree (`syft dir:.`), named `spillway-<image>-<version>-<arch>.spdx.json` and `spillway-source-<version>.spdx.json`.
- **Signing.** cosign (via `sigstore/cosign-installer`, SHA-pinned, with an exact `cosign-release`), keyless only: no keys are created or stored. README gains the `cosign verify` command with the identity above.
- **Pins.** Every action by full commit SHA with the version in a comment, as the repo does; Syft and cosign at exact versions. The release task pins by digest: the Dockerfile's `FROM` lines that are tag-only today (`node:26.13.0-trixie-slim`, `golang:1.27.4-trixie`) as `tag@sha256:…`, every third-party image in `compose.yaml`, `deploy/compose/*/Dockerfile` and `deploy/kind/` as `tag@sha256:…` (the multi-arch index digest), and the kind node image (already by digest). Dependabot keeps updating them (it understands `tag@digest`).
- **Chart.** `deploy/helm/spillway/Chart.yaml` `version: 1.0.0`, `appVersion: 1.0.0`, images defaulting to the ghcr names at `v1.0.0`; the chart is not pushed to a registry in v1 (installed from the repository).
- **What the release task does in this run (prepare only):** writes `CHANGELOG.md`'s 1.0.0 section, `release.yml`, the digest pins and the chart versions on a branch; runs `release.yml` with `dry_run: true` on that branch (`gh workflow run release.yml --ref <branch> -f dry_run=true`), which builds both images for both platforms and the SBOMs and publishes nothing; opens the PR; and **stops**. Creating the `v1.0.0` tag, the `release` environment approval, the ghcr push, the signatures and the GitHub release are the user's, after merging. The M7 release box stays unticked until the user has released.

## Consequences

- A release is one tag push plus one approval; everything else is automated, reproducible from the repository and verifiable with cosign.
- A dry run proves the multi-arch build and the SBOMs without any token that can write.
- Images are immutable references (digest pins); Dependabot PRs move them.
- The workflow needs the repository's `release` environment created by the user (one-time, in GitHub settings), which docs/setup.md's release paragraph will say.

## Alternatives considered

- **Signing with a stored key** — a secret to guard and rotate; keyless ties signatures to the workflow identity.
- **CycloneDX instead of SPDX** — both are fine; SPDX JSON is what GitHub's dependency submission and cosign's `spdxjson` attestation type take without conversion.
- **A `latest` tag** — floating tags defeat digest pinning; users pin `v1.0.0` or a digest.
- **Releasing from a push to main** — the user asked that only a tag or an explicit dispatch can release.
- **Publishing the chart to ghcr as an OCI artifact** — more to sign and verify for a v1 installed from the repository.
