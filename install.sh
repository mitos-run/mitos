#!/bin/sh
# mitos CLI installer.
#
#   curl -fsSL https://get.mitos.run | sh
#
# Downloads the release archive matching this OS/arch from the GitHub release,
# verifies its SHA256 against the published checksums file, extracts the `mitos`
# binary, and installs it. POSIX sh only (this is piped to `sh`); keep it free
# of bashisms so the static linter stays happy.
#
# Environment variables:
#   MITOS_VERSION      version tag to install (default: latest release).
#                      Accepts "v1.2.3" or "1.2.3".
#   MITOS_INSTALL_DIR  install directory (default: /usr/local/bin, falling back
#                      to $HOME/.local/bin if that is not writable).
#   MITOS_SELF_TEST    when set to "1", print the detected os/arch/archive and
#                      exit without downloading. Used by CI to test detection.
#
# Windows is out of scope for this script; use scoop or winget once those
# packages publish (see docs/install.md).

set -eu

REPO="mitos-run/mitos"
BINARY="mitos"

# err prints a message to stderr and exits nonzero.
err() {
	echo "mitos-install: error: $1" >&2
	exit 1
}

# need checks that a required command is on PATH, with actionable remediation.
need() {
	command -v "$1" >/dev/null 2>&1 || err "required command '$1' not found on PATH; install it and re-run"
}

# detect_os maps uname -s to a goreleaser GOOS token.
detect_os() {
	os=$(uname -s)
	case "$os" in
	Linux) echo "linux" ;;
	Darwin) echo "darwin" ;;
	*) err "unsupported OS '$os'; this installer supports linux and darwin. On Windows use scoop or winget (see docs/install.md)" ;;
	esac
}

# detect_arch maps uname -m to a goreleaser GOARCH token.
detect_arch() {
	arch=$(uname -m)
	case "$arch" in
	x86_64 | amd64) echo "amd64" ;;
	aarch64 | arm64) echo "arm64" ;;
	*) err "unsupported architecture '$arch'; this installer supports amd64 and arm64" ;;
	esac
}

# resolve_version echoes the version to install. Honors MITOS_VERSION; otherwise
# queries the GitHub releases API for the latest tag.
resolve_version() {
	if [ -n "${MITOS_VERSION:-}" ]; then
		echo "$MITOS_VERSION"
		return 0
	fi
	api="https://api.github.com/repos/${REPO}/releases/latest"
	# Extract the tag_name without jq: grab the first "tag_name": "..." value.
	tag=$(curl -fsSL "$api" | grep '"tag_name"' | head -n1 | cut -d'"' -f4)
	[ -n "$tag" ] || err "could not resolve the latest release tag from ${api}; set MITOS_VERSION explicitly, or check that a release has been published"
	echo "$tag"
}

# sha256_of prints the sha256 hex digest of file $1 using whichever tool exists.
sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | cut -d' ' -f1
	else
		err "need 'sha256sum' or 'shasum' to verify the download; install one and re-run"
	fi
}

main() {
	os=$(detect_os)
	arch=$(detect_arch)

	# Archive naming mirrors .goreleaser.yaml:
	#   mitos_<version-no-v>_<os>_<arch>.tar.gz
	# The self-test path reports detection without a version, so use a placeholder.
	if [ "${MITOS_SELF_TEST:-}" = "1" ]; then
		echo "os=${os} arch=${arch} archive=${BINARY}_<version>_${os}_${arch}.tar.gz"
		exit 0
	fi

	need curl
	need tar

	version=$(resolve_version)
	# Strip a leading "v" for the archive name; the release URL keeps the tag.
	ver_no_v=${version#v}

	archive="${BINARY}_${ver_no_v}_${os}_${arch}.tar.gz"
	checksums="${BINARY}_${ver_no_v}_checksums.txt"
	base="https://github.com/${REPO}/releases/download/${version}"

	tmp=$(mktemp -d 2>/dev/null || mktemp -d -t mitos-install)
	# Clean up the temp dir on any exit.
	trap 'rm -rf "$tmp"' EXIT INT TERM

	echo "mitos-install: downloading ${archive} (${version}) for ${os}/${arch}" >&2
	curl -fsSL "${base}/${archive}" -o "${tmp}/${archive}" ||
		err "download failed: ${base}/${archive}; verify that release ${version} publishes an archive for ${os}/${arch}"
	curl -fsSL "${base}/${checksums}" -o "${tmp}/${checksums}" ||
		err "checksums download failed: ${base}/${checksums}"

	# Verify the SHA256 of the archive against the published checksums file.
	want=$(grep " ${archive}\$" "${tmp}/${checksums}" | head -n1 | cut -d' ' -f1)
	[ -n "$want" ] || err "checksum for ${archive} not found in ${checksums}; the release may be incomplete"
	got=$(sha256_of "${tmp}/${archive}")
	[ "$want" = "$got" ] || err "checksum mismatch for ${archive}: expected ${want}, got ${got}; aborting"
	echo "mitos-install: checksum verified" >&2

	tar -xzf "${tmp}/${archive}" -C "${tmp}" "${BINARY}" ||
		err "could not extract '${BINARY}' from ${archive}"
	chmod +x "${tmp}/${BINARY}"

	# Resolve the install directory: explicit override, then /usr/local/bin if
	# writable, then $HOME/.local/bin.
	dir=${MITOS_INSTALL_DIR:-}
	if [ -z "$dir" ]; then
		if [ -w /usr/local/bin ] 2>/dev/null; then
			dir="/usr/local/bin"
		else
			dir="${HOME}/.local/bin"
		fi
	fi
	mkdir -p "$dir" || err "could not create install directory ${dir}; set MITOS_INSTALL_DIR to a writable path"

	if ! mv "${tmp}/${BINARY}" "${dir}/${BINARY}" 2>/dev/null; then
		err "could not write ${dir}/${BINARY}; re-run with a writable MITOS_INSTALL_DIR (for example MITOS_INSTALL_DIR=\$HOME/.local/bin), or with sufficient privileges"
	fi

	echo "mitos-install: installed ${BINARY} to ${dir}/${BINARY}" >&2
	case ":${PATH}:" in
	*":${dir}:"*) ;;
	*) echo "mitos-install: note: ${dir} is not on your PATH; add it to use 'mitos' directly" >&2 ;;
	esac

	# Print the installed version as the final line.
	"${dir}/${BINARY}" version
}

main "$@"
