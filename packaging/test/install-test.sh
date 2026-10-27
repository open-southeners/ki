#!/bin/sh
# Exercises install.sh end to end in a plain debian:bookworm-slim container:
# builds a fake release (archive + checksums.txt) from a prebuilt binary, runs a
# fresh install and an upgrade, and asserts the outcome. Docker, git, ssh and
# systemctl are stubbed. Expects /src (repo) and /ki (a linux binary).
set -eu

SRC=${SRC:-/src}
BINARY=${BINARY:-/ki}
WORK=$(mktemp -d)
AGENT_PID=""
trap '[ -n "$AGENT_PID" ] && kill "$AGENT_PID" 2>/dev/null; rm -rf "$WORK"' EXIT
# systemctl is stubbed, so no agent starts; do not wait for its socket.
KI_INSTALL_WAIT=0
export KI_INSTALL_WAIT

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

assert() {
	_msg=$1
	shift
	"$@" || fail "$_msg"
}

apt-get update -qq >/dev/null
apt-get install -y -qq --no-install-recommends ca-certificates >/dev/null

# Stubs for the prerequisites and systemd.
STUBS="$WORK/stubs"
mkdir -p "$STUBS" /run/systemd/system
groupadd docker
for t in docker git ssh; do
	printf '#!/bin/sh\nexit 0\n' >"$STUBS/$t"
done
printf '#!/bin/sh\necho "$*" >>"%s/systemctl.log"\n' "$WORK" >"$STUBS/systemctl"
chmod +x "$STUBS"/*
PATH="$STUBS:$PATH"
export PATH

# Fake release directory.
REL="$WORK/release"
PKG="$WORK/pkg"
mkdir -p "$REL" "$PKG"
cp "$BINARY" "$PKG/ki"
cp "$BINARY" "$PKG/ki-agent"
cp "$SRC/packaging/systemd/ki-agent.service" "$SRC/packaging/config.example.yaml" "$SRC/README.md" "$PKG/"
chmod +x "$PKG/ki" "$PKG/ki-agent"
case "$(uname -m)" in
x86_64) arch=amd64 ;;
*) arch=arm64 ;;
esac
tar -czf "$REL/ki-agent_linux_$arch.tar.gz" -C "$PKG" .
(cd "$REL" && sha256sum "ki-agent_linux_$arch.tar.gz" >checksums.txt)

KI_INSTALL_BASE_URL="file://$REL"
export KI_INSTALL_BASE_URL

echo "== fresh install"
sh "$SRC/install.sh" --add-user nobody
assert "binary not installed" test -x /usr/local/bin/ki
assert "ki-agent is not a symlink" test -L /usr/local/bin/ki-agent
assert "ki-agent does not point at ki" test "$(readlink /usr/local/bin/ki-agent)" = ki
/usr/local/bin/ki version >/dev/null || fail "ki version failed"
/usr/local/bin/ki-agent version >/dev/null || fail "ki-agent version failed"
assert "group missing" getent group ki-agent >/dev/null
assert "user missing" id -u ki-agent >/dev/null
assert "unit not installed" test -f /etc/systemd/system/ki-agent.service
assert "unit does not run ki agent run" grep -q '^ExecStart=/usr/local/bin/ki agent run$' /etc/systemd/system/ki-agent.service
assert "unit lacks User=ki-agent" grep -q '^User=ki-agent$' /etc/systemd/system/ki-agent.service
assert "config not installed" test -f /etc/ki-agent/config.yaml
assert "service user not in docker group" sh -c 'id -nG ki-agent | tr " " "\n" | grep -qx docker'
assert "nobody not in ki-agent group" sh -c 'id -nG nobody | tr " " "\n" | grep -qx ki-agent'
assert "service not enabled and started" grep -q 'enable --now ki-agent.service' "$WORK/systemctl.log"

edge_enabled() {
	awk '/^edge:/ { e = 1; next } /^[^ #]/ { e = 0 } e && /^  enabled:/ { print $2; exit }' /etc/ki-agent/config.yaml
}
assert "default config does not enable the edge" test "$(edge_enabled)" = true

panel_enabled() {
	awk '/^panel:/ { e = 1; next } /^[^ #]/ { e = 0 } e && /^  enabled:/ { print $2; exit }' /etc/ki-agent/config.yaml
}
assert "default config does not enable the panel" test "$(panel_enabled)" = true

echo "== --no-panel on a new config disables the panel"
rm -f /etc/ki-agent/config.yaml
sh "$SRC/install.sh" --no-panel >"$WORK/out.log" 2>&1 || fail "install --no-panel failed"
assert "panel not disabled in the new config" test "$(panel_enabled)" = false
assert "edge was touched by --no-panel" test "$(edge_enabled)" = true
assert "other config keys were damaged" grep -q '^  session_idle: 12h$' /etc/ki-agent/config.yaml
if grep -q 'panel setup' "$WORK/out.log"; then fail "panel setup attempted with the panel disabled"; fi

echo "== --no-panel leaves an existing config alone and says how"
rm -f /etc/ki-agent/config.yaml
sh "$SRC/install.sh" >/dev/null 2>&1
sh "$SRC/install.sh" --no-panel >"$WORK/out.log" 2>&1 || fail "install --no-panel failed"
assert "existing config was edited" test "$(panel_enabled)" = true
assert "no instruction printed" grep -q 'to disable the panel' "$WORK/out.log"

echo "== a fresh install with the panel on points at panel setup when the agent is not up"
rm -f /etc/ki-agent/config.yaml
sh "$SRC/install.sh" >"$WORK/out.log" 2>&1 || fail "install failed"
assert "no socket hint" grep -q 'ki agent panel setup' "$WORK/out.log"

echo "== the installer prints the panel setup link from the running agent"
mkdir -p "$WORK/data"
KI_DATA_DIR="$WORK/data" KI_EDGE_ENABLED=false "$BINARY" agent run >"$WORK/agent.log" 2>&1 &
AGENT_PID=$!
i=0
while [ ! -S /run/ki-agent/ki.sock ] && [ "$i" -lt 30 ]; do
	sleep 1
	i=$((i + 1))
done
assert "agent socket did not appear" test -S /run/ki-agent/ki.sock
rm -f /etc/ki-agent/config.yaml
sh "$SRC/install.sh" >"$WORK/out.log" 2>&1 || fail "install with a running agent failed"
assert "setup link not printed" grep -q '/setup#' "$WORK/out.log"
kill "$AGENT_PID"
wait "$AGENT_PID" 2>/dev/null || true
AGENT_PID=""
rm -f /run/ki-agent/ki.sock

echo "== --no-edge on a new config disables the edge"
rm -f /etc/ki-agent/config.yaml
sh "$SRC/install.sh" --no-edge >"$WORK/out.log" 2>&1 || fail "install --no-edge failed"
assert "edge not disabled in the new config" test "$(edge_enabled)" = false
assert "other config keys were damaged" grep -q '^  webhooks_port: 8090$' /etc/ki-agent/config.yaml

echo "== --no-edge leaves an existing config alone and says how"
rm -f /etc/ki-agent/config.yaml
sh "$SRC/install.sh" >/dev/null 2>&1
echo "# keep" >>/etc/ki-agent/config.yaml
sh "$SRC/install.sh" --no-edge >"$WORK/out.log" 2>&1 || fail "install --no-edge failed"
assert "existing config was edited" test "$(edge_enabled)" = true
assert "no instruction printed" grep -q 'already exists and was not changed' "$WORK/out.log"

echo "== busy port 80 is reported on a fresh config"
printf '#!/bin/sh\necho "LISTEN 0 4096 0.0.0.0:80 0.0.0.0:*"\necho "LISTEN 0 4096 0.0.0.0:22 0.0.0.0:*"\n' >"$STUBS/ss"
chmod +x "$STUBS/ss"
rm -f /usr/local/bin/ki /usr/local/bin/ki-agent /etc/ki-agent/config.yaml
sh "$SRC/install.sh" >"$WORK/out.log" 2>&1 || fail "install failed"
assert "no warning for port 80" grep -q 'already listens on port(s) 80 ' "$WORK/out.log"
if grep -q 'port(s).*22' "$WORK/out.log"; then fail "unrelated port reported"; fi
rm -f "$STUBS/ss" /usr/local/bin/ki /usr/local/bin/ki-agent /etc/ki-agent/config.yaml
sh "$SRC/install.sh" >/dev/null 2>&1 || fail "reinstall failed"

echo "== an existing admin turns the setup link into a hint and does not fail the install"
STUBREL="$WORK/stubrelease"
STUBPKG="$WORK/stubpkg"
mkdir -p "$STUBREL" "$STUBPKG"
cp "$SRC/packaging/systemd/ki-agent.service" "$SRC/packaging/config.example.yaml" "$SRC/README.md" "$STUBPKG/"
cat >"$STUBPKG/ki" <<'STUB'
#!/bin/sh
[ "$1" = agent ] && shift
case "$1" in
version) echo "ki-agent stub" ;;
panel) echo "an admin exists; use ki agent users reset <name>" >&2; exit 1 ;;
tunnel) echo "$*" >>/tmp/ki-stub-tunnel.log ;;
esac
STUB
chmod +x "$STUBPKG/ki"
cp "$STUBPKG/ki" "$STUBPKG/ki-agent"
tar -czf "$STUBREL/ki-agent_linux_$arch.tar.gz" -C "$STUBPKG" .
(cd "$STUBREL" && sha256sum "ki-agent_linux_$arch.tar.gz" >checksums.txt)
mkdir -p /run/ki-agent
python3 -c 'import socket; s = socket.socket(socket.AF_UNIX); s.bind("/run/ki-agent/ki.sock")' 2>/dev/null ||
	perl -e 'use Socket; socket(S, PF_UNIX, SOCK_STREAM, 0); bind(S, sockaddr_un("/run/ki-agent/ki.sock"))' ||
	fail "cannot create a fake socket"
KI_INSTALL_BASE_URL="file://$STUBREL" sh "$SRC/install.sh" >"$WORK/out.log" 2>&1 || fail "install failed when an admin exists"
assert "no hint printed" grep -q 'users reset' "$WORK/out.log"
rm -f /run/ki-agent/ki.sock /usr/local/bin/ki /usr/local/bin/ki-agent /etc/ki-agent/config.yaml
sh "$SRC/install.sh" >/dev/null 2>&1 || fail "reinstall failed"

echo "== --tunnel-token-file writes behind + tunnel config and stores the token after start"
printf 'dHVubmVsLXRva2Vu\n' >"$WORK/tunnel.token"
rm -f /etc/ki-agent/config.yaml /tmp/ki-stub-tunnel.log
KI_INSTALL_BASE_URL="file://$STUBREL" sh "$SRC/install.sh" --tunnel-token-file "$WORK/tunnel.token" >"$WORK/out.log" 2>&1 || fail "install --tunnel-token-file failed"
edge_mode() {
	awk '/^edge:/ { e = 1; next } /^[^ #]/ { e = 0 } e && /^  mode:/ { print $2; exit }' /etc/ki-agent/config.yaml
}
tunnel_enabled() {
	awk '/^tunnel:/ { e = 1; next } /^[^ #]/ { e = 0 } e && /^  enabled:/ { print $2; exit }' /etc/ki-agent/config.yaml
}
assert "edge mode not behind" test "$(edge_mode)" = behind
assert "tunnel not enabled" test "$(tunnel_enabled)" = true
assert "edge disabled by the tunnel option" test "$(edge_enabled)" = true
assert "other config keys were damaged" grep -q '^  metrics_port: 20241$' /etc/ki-agent/config.yaml
assert "token not stored through the CLI" grep -qx "tunnel configure --token-file $WORK/tunnel.token" /tmp/ki-stub-tunnel.log
if grep -q 'dHVubmVsLXRva2Vu' "$WORK/out.log"; then fail "the token was printed"; fi
if grep -q 'port(s)' "$WORK/out.log"; then fail "port warning shown although the tunnel needs no public ports"; fi

echo "== --tunnel-token-file on an existing config only stores the token and says how"
echo "# keep" >>/etc/ki-agent/config.yaml
sed -i 's/^  mode: behind$/  mode: front/' /etc/ki-agent/config.yaml
rm -f /tmp/ki-stub-tunnel.log
KI_INSTALL_BASE_URL="file://$STUBREL" sh "$SRC/install.sh" --tunnel-token-file "$WORK/tunnel.token" >"$WORK/out.log" 2>&1 || fail "install --tunnel-token-file failed"
assert "existing config was edited" test "$(edge_mode)" = front
assert "no instruction printed" grep -q 'for the tunnel set edge.mode: behind' "$WORK/out.log"
assert "token not stored" grep -q 'tunnel configure' /tmp/ki-stub-tunnel.log

echo "== --tunnel-token-file rejects bad input before changing anything"
if sh "$SRC/install.sh" --tunnel-token-file "$WORK/missing.token" >"$WORK/out.log" 2>&1; then fail "missing token file accepted"; fi
if sh "$SRC/install.sh" --tunnel-token-file "$WORK/tunnel.token" --no-edge >"$WORK/out.log" 2>&1; then fail "--no-edge with the tunnel accepted"; fi
if sh "$SRC/install.sh" --tunnel-token-file "$WORK/tunnel.token" --no-start >"$WORK/out.log" 2>&1; then fail "--no-start with the tunnel accepted"; fi
rm -f /run/ki-agent/ki.sock /usr/local/bin/ki /usr/local/bin/ki-agent /etc/ki-agent/config.yaml /tmp/ki-stub-tunnel.log
sh "$SRC/install.sh" >/dev/null 2>&1 || fail "reinstall failed"

echo "== upgrade keeps edited config"
echo "# edited by the admin" >>/etc/ki-agent/config.yaml
: >"$WORK/systemctl.log"
sh "$SRC/install.sh"
assert "config was overwritten" grep -q '^# edited by the admin$' /etc/ki-agent/config.yaml
assert "service not restarted on upgrade" grep -q 'restart ki-agent.service' "$WORK/systemctl.log"
assert "binary no longer runs" /usr/local/bin/ki version >/dev/null
assert "ki-agent link lost on upgrade" test -L /usr/local/bin/ki-agent

echo "== upgrading a 1.0.x install (plain ki-agent file) switches to ki plus a symlink"
rm -f /usr/local/bin/ki /usr/local/bin/ki-agent
cp "$BINARY" /usr/local/bin/ki-agent
: >"$WORK/systemctl.log"
sh "$SRC/install.sh" >"$WORK/out.log" 2>&1 || fail "upgrade from 1.0.x failed"
assert "ki not installed" test -x /usr/local/bin/ki
assert "ki-agent still a plain file" test -L /usr/local/bin/ki-agent
assert "ki-agent does not point at ki" test "$(readlink /usr/local/bin/ki-agent)" = ki
assert "no leftover temp files" test -z "$(ls /usr/local/bin | grep '\.new\.')"
assert "service not restarted on upgrade" grep -q 'restart ki-agent.service' "$WORK/systemctl.log"

echo "== an older release (only ki-agent in the archive) installs as before"
OLDREL="$WORK/oldrelease"
OLDPKG="$WORK/oldpkg"
mkdir -p "$OLDREL" "$OLDPKG"
cp "$SRC/packaging/systemd/ki-agent.service" "$SRC/packaging/config.example.yaml" "$SRC/README.md" "$OLDPKG/"
cp "$BINARY" "$OLDPKG/ki-agent"
tar -czf "$OLDREL/ki-agent_linux_$arch.tar.gz" -C "$OLDPKG" .
(cd "$OLDREL" && sha256sum "ki-agent_linux_$arch.tar.gz" >checksums.txt)
rm -f /usr/local/bin/ki /usr/local/bin/ki-agent
KI_INSTALL_BASE_URL="file://$OLDREL" sh "$SRC/install.sh" >"$WORK/out.log" 2>&1 || fail "install of an older release failed"
assert "ki-agent is not a plain file" test -f /usr/local/bin/ki-agent -a ! -L /usr/local/bin/ki-agent
assert "ki was created for an older release" test ! -e /usr/local/bin/ki
rm -f /usr/local/bin/ki-agent
sh "$SRC/install.sh" >/dev/null 2>&1 || fail "reinstall failed"

echo "== tampered archive is rejected"
echo tampered >>"$REL/ki-agent_linux_$arch.tar.gz"
rm -f /usr/local/bin/ki /usr/local/bin/ki-agent
if sh "$SRC/install.sh" >"$WORK/out.log" 2>&1; then
	fail "install succeeded with a bad checksum"
fi
grep -q 'checksum mismatch' "$WORK/out.log" || fail "no checksum mismatch message"
assert "binary installed despite bad checksum" test ! -e /usr/local/bin/ki

echo "install test passed"
