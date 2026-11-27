#!/usr/bin/env bash
# The release publish (ADR-0076, ADR-0092): run by release.yml's publish job
# only, after make release-build, logged in to the registry.
#   make release-publish RELEASE_VERSION=1.0.0
# For each image: pushes dist/oci/<image> as built (the bytes the SBOMs
# describe) as v<version>, tags it <major>.<minor>, signs the index and
# both platform images with cosign, attests each platform image's SBOM, and
# verifies both. Appends <image>@<index digest> to dist/images.txt.
#
# REGISTRY defaults to ghcr.io/ankitbisht23. Signing is keyless (the
# workflow's GitHub OIDC token, Fulcio, Rekor), verified against the release
# workflow's identity for the tag. COSIGN_SIGN_FLAGS and
# COSIGN_VERIFY_FLAGS, when set, replace that identity (ADR-0092's local dry
# run: a localhost registry, a throwaway key, no transparency log).
set -euo pipefail
: "${VERSION:?}" "${BIN:?}"
cd "$(dirname "$0")/../.."

registry=${REGISTRY:-ghcr.io/ankitbisht23}
identity=https://github.com/ankitbisht23/spillway/.github/workflows/release.yml@refs/tags/v$VERSION
read -ra sign <<<"${COSIGN_SIGN_FLAGS:-}"
read -ra verify <<<"${COSIGN_VERIFY_FLAGS:---certificate-identity $identity --certificate-oidc-issuer https://token.actions.githubusercontent.com}"
minor=${VERSION%.*}

: >dist/images.txt
for image in gateway controlplane; do
	ref=$registry/spillway-$image
	"$BIN/crane" push "dist/oci/$image" "$ref:v$VERSION" --image-refs "dist/$image.ref"
	digest=$(sed -n 's/.*@\(sha256:[0-9a-f]\{64\}\)$/\1/p' "dist/$image.ref")
	test -n "$digest" || { echo "no digest in dist/$image.ref"; exit 1; }
	"$BIN/crane" tag "$ref@$digest" "$minor"

	"$BIN/cosign" sign --yes --recursive ${sign[@]+"${sign[@]}"} "$ref@$digest"
	"$BIN/cosign" verify "${verify[@]}" "$ref@$digest" >/dev/null
	for arch in amd64 arm64; do
		platform=$("$BIN/crane" digest --platform "linux/$arch" "$ref@$digest")
		"$BIN/cosign" verify "${verify[@]}" "$ref@$platform" >/dev/null
		"$BIN/cosign" attest --yes ${sign[@]+"${sign[@]}"} --type spdxjson \
			--predicate "dist/sbom/spillway-$image-$VERSION-$arch.spdx.json" "$ref@$platform"
		"$BIN/cosign" verify-attestation "${verify[@]}" --type spdxjson "$ref@$platform" >/dev/null
	done
	echo "$ref@$digest" >>dist/images.txt
	echo "$ref:v$VERSION ($minor) $digest: signed, SBOMs attested, verified"
done
