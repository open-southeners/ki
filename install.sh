#!/bin/sh
# ki-agent installer.
#
#   curl -fsSL https://github.com/open-southeners/ki-agent/releases/latest/download/install.sh | sudo sh
#   curl -fsSL .../install.sh | sudo sh -s -- --version v1.2.3 --enroll
#
# Downloads the release archive for this machine, verifies its sha256 checksum
# (mandatory) and, when cosign is installed, the checksums' signature, then
# installs the binary, the systemd unit and a default config. Run with --help
# for the options. Re-running upgrades the binary and keeps /etc/ki-agent/config.yaml.
#
# All logic lives in main(), which is invoked on the last line, so a truncated
# download can never execute partially.

set -eu

DEFAULT_BASE_URL="https://github.com/open-southeners/ki-agent/releases"
SIGN_IDENTITY_REGEXP='^https://github\.com/open-southeners/ki-agent/\.github/workflows/release\.yml@refs/tags/v'
SIGN_OIDC_ISSUER="https://token.actions.githubusercontent.com"

BIN_PATH="/usr/local/bin/ki-agent"
UNIT_PATH="/etc/systemd/system/ki-agent.service"
CONFIG_DIR="/etc/ki-agent"
CONFIG_PATH="/etc/ki-agent/config.yaml"
SOCKET_PATH="/run/ki-agent/ki.sock"
SVC_USER="ki-agent"
SVC_GROUP="ki-agent"

TMPDIR_KI=""

usage() {
	cat <<'USAGE'
Usage: install.sh [options]

Options:
  --version vX.Y.Z   Install this release (default: latest).
  --with-docker      Install Docker Engine (via https://get.docker.com) if it
                     or the compose plugin is missing.
  --enroll [URL]     After installing, run "ki-agent enroll [URL]" to connect
                     this server to a control plane.
  --no-edge          Disable the managed reverse proxy (edge) in a newly
                     written config (edge.enabled: false). By default the
                     edge binds ports 80 and 443.
  --tunnel-token-file FILE
                     Expose the server through a Cloudflare Tunnel: a newly
                     written config gets edge.mode: behind and
                     tunnel.enabled: true, and after the service starts the
                     token in FILE is stored with "ki-agent tunnel configure".
                     Create the tunnel in the Cloudflare dashboard first and
                     point its public hostnames at http://localhost:8480.
                     With an existing config only the token is stored; set
                     edge.mode and tunnel.enabled there yourself.
  --no-panel         Disable the web panel in a newly written config
                     (panel.enabled: false). By default the installer prints a
                     one-time link to create the panel admin after starting
                     the service.
  --no-start         Install and enable the service but do not start it.
  --add-user USER    Add USER to the ki-agent group so it can use the CLI
                     (default: the user who ran sudo).
  --help             Show this help.

Environment:
  KI_INSTALL_BASE_URL  Download directory URL holding the release files
                       (https://, http:// or file://); for testing.
  KI_INSTALL_WAIT      Seconds to wait for the agent socket after starting the
                       service (default 15); for testing.
USAGE
}

log() {
	printf '%s\n' "ki-agent install: $*"
}

warn() {
	printf '%s\n' "ki-agent install: warning: $*" >&2
}

die() {
	printf '%s\n' "ki-agent install: error: $*" >&2
	exit 1
}

cleanup() {
	if [ -n "$TMPDIR_KI" ] && [ -d "$TMPDIR_KI" ]; then
		rm -rf "$TMPDIR_KI"
	fi
}

have() {
	command -v "$1" >/dev/null 2>&1
}

# pkg_hint prints the distro-appropriate command for installing the given packages.
pkg_hint() {
	if have apt-get; then
		printf 'apt-get install -y %s' "$*"
	elif have dnf; then
		printf 'dnf install -y %s' "$*"
	elif have yum; then
		printf 'yum install -y %s' "$*"
	else
		printf 'install: %s' "$*"
	fi
}

# fetch URL DEST downloads (or copies, for file://) a file.
fetch() {
	_url=$1
	_dest=$2
	case "$_url" in
	file://*)
		cp "${_url#file://}" "$_dest" || return 1
		;;
	*)
		if have curl; then
			curl -fsSL --retry 3 -o "$_dest" "$_url" || return 1
		elif have wget; then
			wget -q -O "$_dest" "$_url" || return 1
		else
			die "curl or wget is required"
		fi
		;;
	esac
}

# sha256_of FILE prints the hex sha256 digest of FILE.
sha256_of() {
	if have sha256sum; then
		sha256sum "$1" | awk '{print $1}'
	elif have shasum; then
		shasum -a 256 "$1" | awk '{print $1}'
	else
		die "sha256sum or shasum is required to verify the download"
	fi
}

# verify_checksum DIR NAME checks DIR/NAME against its entry in DIR/checksums.txt.
verify_checksum() {
	_expected=$(awk -v n="$2" '$2 == n || $2 == "*" n { print $1; exit }' "$1/checksums.txt")
	[ -n "$_expected" ] || die "no checksum for $2 in checksums.txt"
	_actual=$(sha256_of "$1/$2")
	[ -n "$_actual" ] || die "could not compute the checksum of $2"
	if [ "$_expected" != "$_actual" ]; then
		die "checksum mismatch for $2 (expected $_expected, got $_actual); refusing to install"
	fi
	log "checksum verified for $2"
}

# verify_signature DIR verifies checksums.txt with cosign when it is installed.
verify_signature() {
	if ! have cosign; then
		log "cosign not installed; skipping signature verification (checksum still verified)"
		return 0
	fi
	fetch "$BASE/checksums.txt.sigstore.json" "$1/checksums.txt.sigstore.json" ||
		die "cosign is installed but the signature bundle could not be downloaded"
	cosign verify-blob \
		--bundle "$1/checksums.txt.sigstore.json" \
		--certificate-identity-regexp "$SIGN_IDENTITY_REGEXP" \
		--certificate-oidc-issuer "$SIGN_OIDC_ISSUER" \
		"$1/checksums.txt" >/dev/null 2>&1 ||
		die "signature verification of checksums.txt failed"
	log "signature verified with cosign"
}

# listening_web_ports prints the ports 80 and 443 that something listens on,
# one per line; it prints nothing when neither is taken or when neither ss nor
# netstat is available.
listening_web_ports() {
	if have ss; then
		_listing=$(ss -H -ltn 2>/dev/null) || return 0
	elif have netstat; then
		_listing=$(netstat -ltn 2>/dev/null) || return 0
	else
		return 0
	fi
	printf '%s\n' "$_listing" | awk '{ for (i = 1; i <= NF; i++) if ($i ~ /[:.](80|443)$/) { n = split($i, a, /[:.]/); print a[n]; break } }' | sort -u
}

# nologin_shell prints the path of a shell that refuses logins.
nologin_shell() {
	for _s in /usr/sbin/nologin /sbin/nologin /usr/bin/nologin /bin/false; do
		if [ -x "$_s" ]; then
			printf '%s' "$_s"
			return 0
		fi
	done
	printf '%s' /bin/false
}

# has_systemd reports whether systemd is the running init.
has_systemd() {
	have systemctl && [ -d /run/systemd/system ]
}

# replace_file SRC DEST MODE atomically installs SRC as DEST.
replace_file() {
	_tmp="$2.new.$$"
	install -m "$3" "$1" "$_tmp"
	mv -f "$_tmp" "$2"
}

# wait_for_socket waits for the agent socket (up to KI_INSTALL_WAIT seconds).
wait_for_socket() {
	_n=${KI_INSTALL_WAIT:-15}
	_i=0
	while [ ! -S "$SOCKET_PATH" ] && [ "$_i" -lt "$_n" ]; do
		sleep 1
		_i=$((_i + 1))
	done
	[ -S "$SOCKET_PATH" ]
}

# setup_panel prints the one-time link that creates the panel admin. It never
# fails the install: with an admin already present it prints a hint instead.
setup_panel() {
	if ! wait_for_socket; then
		warn "the agent socket did not appear; create the panel admin later with: ki-agent panel setup"
		return 0
	fi
	if panel_out=$("$BIN_PATH" panel setup 2>&1); then
		log "open this link to create the panel admin (password and authenticator app):"
		printf '%s\n' "$panel_out"
	elif printf '%s' "$panel_out" | grep -q 'admin exists'; then
		log "the panel already has an admin; to issue a new setup link run: ki-agent users reset <username>"
	else
		warn "could not create the panel setup link ($panel_out); run later: ki-agent panel setup"
	fi
	return 0
}

# panel_disabled succeeds when the config turns the panel off.
panel_disabled() {
	[ "$(awk '
		/^panel:/ { insec = 1; next }
		/^[^ #]/ { insec = 0 }
		insec && /^  enabled:/ { print $2; exit }
	' "$CONFIG_PATH" 2>/dev/null)" = false ]
}

# attached runs a command with the terminal on stdin even when the script was piped in.
attached() {
	if [ ! -t 0 ] && ( : </dev/tty ) 2>/dev/null; then
		"$@" </dev/tty
	else
		"$@"
	fi
}

main() {
	version=""
	with_docker=0
	do_enroll=0
	enroll_url=""
	no_start=0
	no_edge=0
	no_panel=0
	tunnel_token_file=""
	add_user=${SUDO_USER:-}

	while [ $# -gt 0 ]; do
		case "$1" in
		--version)
			[ $# -ge 2 ] || die "--version needs a value such as v1.2.3"
			version=$2
			shift 2
			;;
		--with-docker)
			with_docker=1
			shift
			;;
		--enroll)
			do_enroll=1
			shift
			if [ $# -gt 0 ]; then
				case "$1" in
				-*) ;;
				*)
					enroll_url=$1
					shift
					;;
				esac
			fi
			;;
		--no-edge)
			no_edge=1
			shift
			;;
		--no-panel)
			no_panel=1
			shift
			;;
		--tunnel-token-file)
			[ $# -ge 2 ] || die "--tunnel-token-file needs a file name"
			tunnel_token_file=$2
			shift 2
			;;
		--no-start)
			no_start=1
			shift
			;;
		--add-user)
			[ $# -ge 2 ] || die "--add-user needs a user name"
			add_user=$2
			shift 2
			;;
		-h | --help)
			usage
			return 0
			;;
		*)
			usage >&2
			die "unknown option: $1"
			;;
		esac
	done

	if [ -n "$version" ]; then
		printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$' ||
			die "invalid version '$version'; expected something like v1.2.3"
	fi
	if [ -n "$tunnel_token_file" ]; then
		[ -f "$tunnel_token_file" ] && [ -r "$tunnel_token_file" ] || die "--tunnel-token-file: cannot read $tunnel_token_file"
		[ "$no_edge" -eq 0 ] || die "--tunnel-token-file needs the edge; drop --no-edge"
		[ "$no_start" -eq 0 ] || die "--tunnel-token-file needs the service running; drop --no-start"
	fi
	if [ "$do_enroll" -eq 1 ] && [ "$no_start" -eq 1 ]; then
		die "--enroll needs the service running; drop --no-start"
	fi

	[ "$(uname -s)" = "Linux" ] || die "only Linux is supported"
	if [ "$(id -u)" -ne 0 ]; then
		die "must run as root; re-run with sudo, e.g. curl -fsSL <url>/install.sh | sudo sh -s -- [options]"
	fi

	case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) die "unsupported architecture: $(uname -m)" ;;
	esac

	if [ -n "$add_user" ] && [ "$add_user" != "root" ]; then
		id -u "$add_user" >/dev/null 2>&1 || die "user '$add_user' does not exist"
	else
		add_user=""
	fi

	# Prerequisites: checked before anything is downloaded or changed.
	missing=""
	have git || missing="$missing git"
	have ssh || missing="$missing openssh-client"
	if [ -n "$missing" ]; then
		# shellcheck disable=SC2086 # intentional word splitting of the package list
		die "missing required tools:$missing; install them with: $(pkg_hint $missing)"
	fi
	need_docker=0
	if ! have docker || ! docker compose version >/dev/null 2>&1; then
		need_docker=1
		if [ "$with_docker" -eq 0 ]; then
			die "Docker Engine with the compose plugin is required; install it (https://docs.docker.com/engine/install/) or re-run with --with-docker"
		fi
	fi

	if [ -n "${KI_INSTALL_BASE_URL:-}" ]; then
		BASE=${KI_INSTALL_BASE_URL%/}
	elif [ -n "$version" ]; then
		BASE="$DEFAULT_BASE_URL/download/$version"
	else
		BASE="$DEFAULT_BASE_URL/latest/download"
	fi

	TMPDIR_KI=$(mktemp -d)
	trap cleanup EXIT
	trap 'exit 1' HUP INT TERM

	archive="ki-agent_linux_${arch}.tar.gz"
	log "downloading $archive from $BASE"
	fetch "$BASE/checksums.txt" "$TMPDIR_KI/checksums.txt" || die "could not download checksums.txt from $BASE"
	fetch "$BASE/$archive" "$TMPDIR_KI/$archive" || die "could not download $archive from $BASE"
	verify_signature "$TMPDIR_KI"
	verify_checksum "$TMPDIR_KI" "$archive"

	mkdir "$TMPDIR_KI/extract"
	tar -xzf "$TMPDIR_KI/$archive" -C "$TMPDIR_KI/extract"
	for f in ki-agent ki-agent.service config.example.yaml; do
		[ -f "$TMPDIR_KI/extract/$f" ] || die "archive is missing $f"
	done

	# Everything below changes the system; the download is verified.
	if [ "$need_docker" -eq 1 ]; then
		log "installing Docker Engine via https://get.docker.com"
		fetch "https://get.docker.com" "$TMPDIR_KI/get-docker.sh" || die "could not download the Docker installer"
		sh "$TMPDIR_KI/get-docker.sh"
		if ! have docker || ! docker compose version >/dev/null 2>&1; then
			die "Docker or the compose plugin is still missing after installation"
		fi
	fi

	if ! getent group "$SVC_GROUP" >/dev/null 2>&1; then
		log "creating group $SVC_GROUP"
		groupadd --system "$SVC_GROUP"
	fi
	if ! id -u "$SVC_USER" >/dev/null 2>&1; then
		log "creating user $SVC_USER"
		useradd --system --gid "$SVC_GROUP" --no-create-home --home-dir /nonexistent \
			--shell "$(nologin_shell)" "$SVC_USER"
	fi
	if getent group docker >/dev/null 2>&1; then
		usermod -aG docker "$SVC_USER"
	else
		warn "no docker group found; the service cannot reach the Docker socket until it exists"
	fi
	if [ -n "$add_user" ]; then
		usermod -aG "$SVC_GROUP" "$add_user"
		log "added $add_user to the $SVC_GROUP group (takes effect at next login)"
	fi

	upgrade=0
	if [ -e "$BIN_PATH" ]; then
		upgrade=1
	fi
	mkdir -p "$(dirname "$BIN_PATH")"
	replace_file "$TMPDIR_KI/extract/ki-agent" "$BIN_PATH" 0755
	installed_version=$("$BIN_PATH" version 2>&1 | head -n 1) || installed_version=""

	mkdir -p "$(dirname "$UNIT_PATH")"
	replace_file "$TMPDIR_KI/extract/ki-agent.service" "$UNIT_PATH" 0644

	install -d -m 0755 "$CONFIG_DIR"
	config_note="kept existing $CONFIG_PATH"
	if [ ! -e "$CONFIG_PATH" ]; then
		install -m 0640 -g "$SVC_GROUP" "$TMPDIR_KI/extract/config.example.yaml" "$CONFIG_PATH"
		config_note="wrote default $CONFIG_PATH"
		if [ "$no_edge" -eq 1 ]; then
			# Flip the first "enabled:" key inside the edge: section.
			awk '
				/^edge:/ { inedge = 1; print; next }
				/^[^ #]/ { inedge = 0 }
				inedge && !done && /^  enabled:/ { print "  enabled: false"; done = 1; next }
				{ print }
			' "$CONFIG_PATH" >"$CONFIG_PATH.new.$$"
			cat "$CONFIG_PATH.new.$$" >"$CONFIG_PATH"
			rm -f "$CONFIG_PATH.new.$$"
			config_note="$config_note (edge disabled)"
		fi
		if [ "$no_panel" -eq 1 ]; then
			# Flip the first "enabled:" key inside the panel: section.
			awk '
				/^panel:/ { inpanel = 1; print; next }
				/^[^ #]/ { inpanel = 0 }
				inpanel && !done && /^  enabled:/ { print "  enabled: false"; done = 1; next }
				{ print }
			' "$CONFIG_PATH" >"$CONFIG_PATH.new.$$"
			cat "$CONFIG_PATH.new.$$" >"$CONFIG_PATH"
			rm -f "$CONFIG_PATH.new.$$"
			config_note="$config_note (panel disabled)"
		fi
		if [ -n "$tunnel_token_file" ]; then
			# Behind mode (the tunnel reaches the edge on loopback) and the tunnel on.
			awk '
				/^edge:/ { insec = "edge"; print; next }
				/^tunnel:/ { insec = "tunnel"; print; next }
				/^[^ #]/ { insec = "" }
				insec == "edge" && !edone && /^  mode:/ { print "  mode: behind"; edone = 1; next }
				insec == "tunnel" && !tdone && /^  enabled:/ { print "  enabled: true"; tdone = 1; next }
				{ print }
			' "$CONFIG_PATH" >"$CONFIG_PATH.new.$$"
			cat "$CONFIG_PATH.new.$$" >"$CONFIG_PATH"
			rm -f "$CONFIG_PATH.new.$$"
			config_note="$config_note (edge behind, tunnel enabled)"
		fi
		if [ "$no_edge" -eq 0 ] && [ "$upgrade" -eq 0 ] && [ -z "$tunnel_token_file" ]; then
			taken=$(listening_web_ports | tr '\n' ' ')
			if [ -n "$taken" ]; then
				warn "something already listens on port(s) ${taken}which the edge proxy needs (80 and 443); the edge will report an error until they are free. Stop that service, change edge.http_port/edge.https_port in $CONFIG_PATH, or reinstall with --no-edge"
			fi
		fi
	else
		if [ "$no_edge" -eq 1 ]; then
			warn "$CONFIG_PATH already exists and was not changed; to disable the edge set \"enabled: false\" under \"edge:\" there (or KI_EDGE_ENABLED=false) and restart the service"
		fi
		if [ -n "$tunnel_token_file" ]; then
			warn "$CONFIG_PATH already exists and was not changed; for the tunnel set edge.mode: behind and tunnel.enabled: true there and restart the service"
		fi
		if [ "$no_panel" -eq 1 ]; then
			warn "$CONFIG_PATH already exists and was not changed; to disable the panel set \"enabled: false\" under \"panel:\" there (or KI_PANEL_ENABLED=false) and restart the service"
		fi
	fi

	service_state="not managed (systemd not running)"
	if has_systemd; then
		systemctl daemon-reload
		if [ "$no_start" -eq 1 ]; then
			systemctl enable ki-agent.service
			service_state="enabled, not started (--no-start)"
		elif [ "$upgrade" -eq 1 ]; then
			systemctl enable ki-agent.service
			systemctl restart ki-agent.service
			service_state="restarted"
		else
			systemctl enable --now ki-agent.service
			service_state="enabled and started"
		fi
	else
		warn "systemd is not running; start the agent yourself with: $BIN_PATH run"
	fi

	log "installed ${installed_version:-ki-agent}"
	log "service: $service_state; config: $config_note"
	if [ -n "$add_user" ]; then
		log "log out and back in so $add_user can use the ki-agent CLI (group $SVC_GROUP)"
	else
		log "users must be in the $SVC_GROUP group to use the ki-agent CLI: usermod -aG $SVC_GROUP <user>"
	fi

	if has_systemd && [ "$no_start" -eq 0 ] && ! panel_disabled; then
		setup_panel
	fi

	if [ -n "$tunnel_token_file" ]; then
		if ! has_systemd; then
			warn "skipping the tunnel token: the service is not running; run later: ki-agent tunnel configure --token-file FILE"
		else
			wait_for_socket || true
			if "$BIN_PATH" tunnel configure --token-file "$tunnel_token_file" >/dev/null; then
				log "stored the Cloudflare Tunnel token; check it with: ki-agent tunnel status"
			else
				warn "could not store the tunnel token; run later: ki-agent tunnel configure --token-file FILE"
			fi
		fi
	fi

	if [ "$do_enroll" -eq 1 ]; then
		if ! has_systemd; then
			warn "skipping enrollment: the service is not running; run later: ki-agent enroll"
		else
			wait_for_socket || true
			if [ -n "$enroll_url" ]; then
				attached "$BIN_PATH" enroll "$enroll_url"
			else
				attached "$BIN_PATH" enroll
			fi
		fi
	else
		log "next: ki-agent projects create <git-url>   or   ki-agent enroll [url]"
	fi
}

main "$@"
