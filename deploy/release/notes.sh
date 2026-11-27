#!/usr/bin/env bash
# Prints CHANGELOG.md's section for one version, without its heading: the
# GitHub release notes (ADR-0076). Fails when there is no such section.
#   deploy/release/notes.sh 1.0.0
set -euo pipefail
: "${1:?usage: notes.sh <version>}"
cd "$(dirname "$0")/../.."
awk -v v="$1" '
	index($0, "## [" v "]") == 1 { found = 1; next }
	found && (/^## \[/ || /^\[[^]]+\]: /) { exit }
	found { print }
	END { if (!found) { print "CHANGELOG.md has no section [" v "]" > "/dev/stderr"; exit 1 } }
' CHANGELOG.md | sed -e '/./,$!d' | sed -e ':a' -e '/^\n*$/{$d;N;ba' -e '}'
