#!/usr/bin/env bash
# The release build (ADR-0076, ADR-0092); pushes and signs nothing.
#   make release-build RELEASE_VERSION=1.0.0
# Writes dist/ (gitignored), replacing what was there:
#   oci/<image>/      OCI layout: one index of linux/amd64 and linux/arm64,
#                     the bytes release-publish pushes
#   sbom/spillway-<image>-<version>-<arch>.spdx.json   per image and platform
#   sbom/spillway-source-<version>.spdx.json           the source tree
#   release-notes.md  CHANGELOG.md's section for the chart's appVersion
#   SHA256SUMS        of the SBOMs
set -euo pipefail
: "${VERSION:?}" "${BIN:?}"
cd "$(dirname "$0")/../.."

images=(gateway controlplane)
arches=(amd64 arm64)

rm -rf dist && mkdir -p dist/oci dist/sbom
for image in "${images[@]}"; do
	# No provenance attestation in the index: the release's attestations are
	# cosign's (ADR-0092).
	docker buildx build --target "$image" --platform linux/amd64,linux/arm64 \
		--build-arg VERSION="v$VERSION" --provenance=false \
		--output "type=oci,dest=dist/oci/$image,tar=false" .
	for arch in "${arches[@]}"; do
		"$BIN/syft" scan "oci-dir:dist/oci/$image" --platform "linux/$arch" -q \
			--source-name "ghcr.io/ankitbisht23/spillway-$image" --source-version "v$VERSION" \
			-o "spdx-json=dist/sbom/spillway-$image-$VERSION-$arch.spdx.json"
	done
done

# The Go module and the pnpm lockfile, not installed or built files.
"$BIN/syft" scan dir:. -q --source-name spillway --source-version "v$VERSION" \
	--exclude './.git' --exclude './bin' --exclude './dist' --exclude './**/node_modules' \
	--exclude './web/dist' --exclude './internal/controlplane/webui/dist' \
	-o "spdx-json=dist/sbom/spillway-source-$VERSION.spdx.json"

app=$(awk -F'"' '/^appVersion:/ {print $2}' deploy/helm/spillway/Chart.yaml)
deploy/release/notes.sh "$app" >dist/release-notes.md
(cd dist/sbom && shasum -a 256 -- *.spdx.json) >dist/SHA256SUMS

# Each SBOM names the binary's Go module and its platform.
for image in "${images[@]}"; do
	for arch in "${arches[@]}"; do
		f=dist/sbom/spillway-$image-$VERSION-$arch.spdx.json
		jq -e '.spdxVersion == "SPDX-2.3" and any(.packages[]; .name == "github.com/ankitbisht23/spillway")' "$f" >/dev/null ||
			{ echo "$f: not an SPDX 2.3 SBOM of the spillway module"; exit 1; }
	done
done
echo "dist/: $(ls dist/sbom | wc -l | tr -d ' ') SBOMs, release notes for $app, images ${images[*]} for ${arches[*]}"
