# 0092. Release: implementation details

- **Status:** Accepted
- **Date:** 2026-11-27
- **Requirement IDs:** SEC-10, CP-19, NFR-04, M7 release box

## Context

ADR-0076 (D12) fixes the release: Keep a Changelog, Syft SPDX JSON SBOMs per image and platform plus the source, `release.yml` on `v*` tags or a dispatch that defaults to a dry run, publishing only from a tag behind a `release` environment, cosign keyless signatures and SBOM attestations, digest pins, and in this run only a prepared PR. It leaves the mechanics open: how the tools are installed, how the images get from the build job to the registry, what exactly is signed, how tags relate to the chart, and how much can be dry-run before the workflow is on `main`.

Two facts shape the answer. GitHub runs `workflow_dispatch` only for workflows that exist on the default branch, so `release.yml` cannot be dispatched on `m7-release` before it merges. And the verify-main run on a69b856 failed NFR-04 from two Docker Desktop VM pauses in one run; the user decided on 2026-11-27 to record that as a macOS / Docker Desktop constraint, with no rule change and no re-run, and to say so in this PR.

## Decision

1. **Tools, pinned in the Makefile** (the newest release at least two weeks old on 2026-11-27): Syft v1.57.1 and cosign v3.1.4 as official release binaries checked against SHA-256 sums from their releases' checksum files (both files verified with `cosign verify-blob` against their Sigstore bundles before the sums were copied); crane v0.22.1 and actionlint v1.7.12 with `go install`. Not `anchore/sbom-action` or `sigstore/cosign-installer`: one install path for CI, the release and the laptop, and no third-party action in the job that can write.
2. **One build, pushed as built.** `make release-build` (`deploy/release/build.sh`) builds the `gateway` and `controlplane` targets for `linux/amd64,linux/arm64` into OCI layouts (`--output type=oci,tar=false`) in `dist/oci/`. Syft scans each layout per platform (`syft scan oci-dir:… --platform linux/<arch>`), so each SBOM's container checksum is that platform's manifest digest. The publish job pushes the layouts with `crane push`, which keeps every digest (checked locally: the pushed index digest equals the layout's). The signed and attested bytes are the bytes the SBOMs describe, with no second build. The SEC-10 smoke check (user 65532, read-only root filesystem) runs on a `--load` build of the runner's platform from the same cache, as CI's image job does.
3. **No QEMU** (ADR-0076 named QEMU and Buildx). The Dockerfile cross-compiles and its final stages only copy files, so nothing runs on the target platform during the build; the arm64 image is not executed, as in CI today.
4. **`--provenance=false`.** The index holds the two platform images only; provenance is the cosign certificate, which names the workflow, the tag and the commit.
5. **Signing and attestation** (`deploy/release/publish.sh`): `cosign sign --yes --recursive` on the index digest signs the index and both platform manifests; each platform digest gets its own SBOM as `cosign attest --type spdxjson`; then `cosign verify` and `cosign verify-attestation` check every signature and attestation against the identity `https://github.com/ankitbisht23/spillway/.github/workflows/release.yml@refs/tags/v<version>` and the issuer `https://token.actions.githubusercontent.com`. Tags: `v<version>` and `<major>.<minor>`, no `latest`. `dist/images.txt` lists `<image>@<index digest>` for the release assets. `REGISTRY`, `COSIGN_SIGN_FLAGS` and `COSIGN_VERIFY_FLAGS` exist only so the local dry run (item 10) can point the same script at a localhost registry and a throwaway key.
6. **Versions.** A tag must be `v<major>.<minor>.<patch>` and equal `v` + the chart's `appVersion`, and CHANGELOG.md must have that section (the build job fails otherwise). A dispatch builds `0.0.0-dryrun.<sha12>`, CI `0.0.0-ci.<sha12>`. The binaries' `main.version` is `v<version>`; SBOMs are `spillway-<image>-<version>-<arch>.spdx.json` and `spillway-source-<version>.spdx.json` (version without the `v`). The source SBOM excludes `.git`, `bin`, `dist`, `node_modules` and built UI files, so it lists the Go module and the pnpm lockfile.
7. **Chart.** `version: 1.0.0`, `appVersion: "1.0.0"`; the image helper's default tag becomes `v<appVersion>`, the tag the workflow pushes (an explicit `image.tag` still wins, as kind's `:kind` does).
8. **Digest pins** (`tag@sha256:<multi-arch index digest>`, resolved with `docker buildx imagetools inspect` on 2026-11-27): the Dockerfile's `node` and `golang` stages, every third-party image in `compose.yaml`, `deploy/compose/*/Dockerfile` (busybox included) and `deploy/kind/`, and the Makefile's `PROMETHEUS_IMAGE` (compose's Prometheus, used for promtool). The testcontainers images in `test/integration` (`redis:8.10.3`, `postgres:18.6`) stay tag-only: test code, outside ADR-0076's list. `test/release` fails on any unpinned third-party image.
9. **CI without publishing.** `ci.yml`'s image job runs `make release-build` and uploads the SBOMs, `SHA256SUMS` and the release notes as the `sbom` artifact (7 days); its go job runs `make lint-actions`, which `make lint` also runs. actionlint runs with its shellcheck and pyflakes passes off, so machines with and without shellcheck agree.
10. **The dry run in this run.** Because a dispatch needs the workflow on `main`, the branch was dry-run locally instead: actionlint; `make release-build RELEASE_VERSION=1.0.0` (both images for both platforms, 5 SBOMs, notes, sums); `make release-publish` against a throwaway `registry:3.0.0` on 127.0.0.1 with a throwaway key, a signing config with no transparency log or timestamp service and `HTTPS_PROXY` pointed at a closed port, so nothing could leave the machine; and the keyless `cosign verify` flags checked read-only against a public image signed by GitHub Actions. After merging, `gh workflow run release.yml` runs the dry run on GitHub.
11. **Release notes** are CHANGELOG.md's section without its heading (`deploy/release/notes.sh`). The 1.0.0 section is dated 2026-11-27, the day it was prepared; whoever tags later may change the date in a PR first.
12. **The `release` environment** must exist with the user as required reviewer before the first tag: a job that names a missing environment creates it unprotected. docs/setup.md Step 3 says so and gives the tag commands.
13. **NFR-04 on Docker Desktop for Mac** (the user's decision, 2026-11-27): the README's Known limits and the changelog's Known limitations say that NFR-04 and the other Redis-budget checks can fail a verify run when the VM pauses longer than the 5 ms budget more than once in a run, that ADR-0034 excuses one burst of 1 s or less, and what was seen on a69b856. No pass rule changes.
14. **(P41) Repo layout** (proposed; numbered after P40, which the verify-main step put to the user: let ADR-0034 excuse several short bursts per run instead of one, still open): the `deploy/` line gains `release/` ("…, prometheus/rules/, release/ (release build and publish scripts)"), and the `test/` line gains `release/`.

## Consequences

- A release is one tag push and one approval; the dry run, CI and the laptop run the same `make release-build`.
- What is signed is what was built and scanned; `cosign verify` and `verify-attestation` with the README's commands check it.
- The tools are bumped by hand (Dependabot does not read the Makefile), like kubectl and helm.
- CI's image job does one more multi-arch build of two images (the Go build stage reruns because `VERSION` differs; dependency layers are cached).
- The first GitHub run of `release.yml` happens after merge, as a dry run.

## Alternatives considered

- **Rebuild and `--push` in the publish job** — a second build, whose bytes the SBOMs and smoke checks never saw.
- **BuildKit's own SBOM and provenance attestations** — ADR-0076 chose Syft and cosign; two attestation formats would be two things to verify.
- **Attesting the index instead of each platform image** — an SBOM describes one platform's filesystem.
- **A temporary `push` or `pull_request` trigger to dry-run on the branch** — ADR-0076 forbids both triggers.
- **Signing the dry run's local test with the public Sigstore instance** — it would publish a log entry outside the repository.
