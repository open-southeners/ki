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
SIGN_IDENTITY_REGEXP='^https://github\.com/open-southeners/ki-agent/'
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
  --no-start         Install and enable the service but do not start it.
  --add-user USER    Add USER to the ki-agent group so it can use the CLI
                     (default: the user who ran sudo).
  --help             Show this help.

Environment:
  KI_INSTALL_BASE_URL  Download directory URL holding the release files
                       (https://, http:// or file://); for testing.
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

	if [ "$do_enroll" -eq 1 ]; then
		if ! has_systemd; then
			warn "skipping enrollment: the service is not running; run later: ki-agent enroll"
		else
			i=0
			while [ ! -S "$SOCKET_PATH" ] && [ "$i" -lt 15 ]; do
				sleep 1
				i=$((i + 1))
			done
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
