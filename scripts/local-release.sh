#!/bin/sh
# Builds a release-shaped directory (linux amd64 + arm64 archives, checksums.txt
# and install.sh) from the working tree, for installs before a public release:
#
#   scripts/local-release.sh [VERSION] [OUTDIR]
#   KI_INSTALL_BASE_URL=file:///path/to/dist-local sh dist-local/install.sh
#
# VERSION defaults to `git describe --tags --always --dirty`. OUTDIR defaults to
# ./dist-local. KI_ENROLL_URL, when set, is built in like the release workflow
# does. Needs go and bun (the web panel is embedded into the binary).

set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"

version=${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
out=${2:-"$root/dist-local"}
version=${version#v}
commit=$(git rev-parse HEAD 2>/dev/null || echo unknown)
pkg=github.com/open-southeners/ki/internal/version

command -v go >/dev/null 2>&1 || {
	echo "local-release: go is required" >&2
	exit 1
}
command -v bun >/dev/null 2>&1 || {
	echo "local-release: bun is required" >&2
	exit 1
}

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	else
		shasum -a 256 "$1" | awk '{print $1}'
	fi
}

echo "local-release: building the web panel"
(cd web && bun install --frozen-lockfile && bun run build)

mkdir -p "$out"
out=$(cd "$out" && pwd -P)
case "$out" in
/ | "$root" | "${HOME:-/nonexistent}")
	echo "local-release: refusing to use $out as the output directory" >&2
	exit 1
	;;
esac
if [ -n "$(ls -A "$out")" ] && [ ! -f "$out/.ki-local-release" ]; then
	echo "local-release: $out is not empty and was not created by this script (no .ki-local-release marker); refusing" >&2
	exit 1
fi
# Remove only what this script produces.
rm -f "$out/ki-agent_linux_amd64.tar.gz" "$out/ki-agent_linux_arm64.tar.gz" \
	"$out/checksums.txt" "$out/checksums.txt.sig" "$out/install.sh" "$out/.ki-local-release"
: >"$out/.ki-local-release"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

for arch in amd64 arm64; do
	echo "local-release: building linux/$arch ($version)"
	stage="$work/$arch"
	mkdir -p "$stage"
	CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
		-ldflags "-s -w -X $pkg.Version=$version -X $pkg.Commit=$commit -X $pkg.EnrollURL=${KI_ENROLL_URL:-}" \
		-o "$stage/ki" ./cmd/ki
	# The same binary under the pre-rename name, as a regular file like the
	# goreleaser archives (install.sh links /usr/local/bin/ki-agent to ki).
	cp "$stage/ki" "$stage/ki-agent"
	cp LICENSE README.md "$stage/"
	cp packaging/systemd/ki-agent.service "$stage/ki-agent.service"
	cp packaging/systemd/ki-agent-update.service packaging/systemd/ki-agent-update.path "$stage/"
	cp packaging/config.example.yaml "$stage/config.example.yaml"
	# Same layout as the goreleaser archives: files at the archive root.
	tar -czf "$out/ki-agent_linux_$arch.tar.gz" -C "$stage" \
		ki ki-agent LICENSE README.md ki-agent.service ki-agent-update.service ki-agent-update.path config.example.yaml
done

cp install.sh "$out/install.sh"

# checksums.txt: "<sha256>  <name>" per file, as goreleaser writes it.
: >"$out/checksums.txt"
for f in ki-agent_linux_amd64.tar.gz ki-agent_linux_arm64.tar.gz install.sh; do
	printf '%s  %s\n' "$(sha256_of "$out/$f")" "$f" >>"$out/checksums.txt"
done

# The updater only installs releases with an ed25519 signature over checksums.txt.
if [ -n "${KI_RELEASE_SIGNING_KEY:-}" ]; then
	go run ./tools/release-sign sign --tag "v$version" --in "$out/checksums.txt" --out "$out/checksums.txt.sig"
else
	echo "local-release: KI_RELEASE_SIGNING_KEY not set; skipped checksums.txt.sig (such builds cannot be installed by the updater)"
fi

echo "local-release: wrote $out"
echo "install with: KI_INSTALL_BASE_URL=file://$out sh $out/install.sh"
