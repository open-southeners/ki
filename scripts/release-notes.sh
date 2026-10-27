#!/bin/sh
# Prints the CHANGELOG.md section for a release tag, for use as the GitHub
# release body:
#
#   scripts/release-notes.sh v1.2.3
#
# Takes the body of the "## [1.2.3]" (or "## [1.2.3] - date") heading up to the
# next "## [" heading. Fails when the section is missing or empty.

set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
changelog=${CHANGELOG:-"$root/CHANGELOG.md"}

[ $# -eq 1 ] || {
	echo "usage: release-notes.sh <tag>" >&2
	exit 2
}
version=${1#v}

[ -f "$changelog" ] || {
	echo "release-notes: $changelog not found" >&2
	exit 1
}

notes=$(awk -v ver="$version" '
	/^## \[/ {
		if (found) exit
		h = substr($0, 5)
		sub(/\].*/, "", h)
		if (h == ver) found = 1
		next
	}
	found { print }
' "$changelog")

# An empty body (only blank lines) counts as missing.
if [ -z "$(printf '%s' "$notes" | tr -d ' \t\r\n')" ]; then
	echo "release-notes: no CHANGELOG.md entry for $version" >&2
	exit 1
fi

printf '%s\n' "$notes"
