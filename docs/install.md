# Installing the mitos CLI

The `mitos` command-line interface drives the sandbox lifecycle (create, exec,
file IO, fork, terminate, list) and brings a local dev cluster up or down.

This page covers every install method, what works today versus what arrives with
the first tagged release, the environment variables the installer honors, and how
to verify a download.

## Status of each method

Honesty rule: a method is only listed as working once it actually publishes. The
table below is explicit about which paths work today and which arrive with the
first tagged release.

| Method | Status |
| --- | --- |
| `go install mitos.run/mitos/cmd/mitos@latest` | Works today. Requires a Go toolchain. |
| Build from a checkout (`go build ./cmd/mitos`) | Works today. Requires a Go toolchain. |
| `curl -fsSL https://get.mitos.run \| sh` | Available from the first tagged release. |
| Manual release binary | Available from the first tagged release. |
| Homebrew (`brew install mitos-run/tap/mitos`) | Coming with releases. |
| Debian/RPM packages (`.deb`, `.rpm`) | Coming with releases. |
| Windows (`scoop`, `winget`) | Coming with releases. |

## Works today: Go toolchain

```bash
go install mitos.run/mitos/cmd/mitos@latest
```

This installs `mitos` into `$(go env GOPATH)/bin`. A binary built this way reports
its version as `dev` because it is not built through the release pipeline:

```bash
mitos version
# mitos dev (commit none, built unknown, linux/amd64)
```

Or build from a checkout:

```bash
git clone https://github.com/mitos-run/mitos
cd mitos
go build -o mitos ./cmd/mitos/
```

## From the first tagged release: install script

```bash
curl -fsSL https://get.mitos.run | sh
```

The script (`install.sh` at the repo root) detects your OS and architecture,
resolves the version, downloads the matching archive plus the checksums file from
the GitHub release, verifies the SHA256, extracts the `mitos` binary, installs it,
and prints the installed version. It is POSIX `sh` and supports linux and darwin on
amd64 and arm64.

Windows is out of scope for the script; use scoop or winget once those publish.

### Environment variables the script honors

| Variable | Default | Purpose |
| --- | --- | --- |
| `MITOS_VERSION` | latest release | Tag to install. Accepts `v1.2.3` or `1.2.3`. |
| `MITOS_INSTALL_DIR` | `/usr/local/bin`, falling back to `$HOME/.local/bin` | Install directory. The script falls back automatically when `/usr/local/bin` is not writable. |
| `MITOS_SELF_TEST` | unset | When `1`, prints the detected `os`, `arch`, and archive name, then exits without downloading. Used by CI. |

Examples:

```bash
# Pin a version and install into a user-writable directory:
MITOS_VERSION=v0.11.0 MITOS_INSTALL_DIR="$HOME/.local/bin" \
  curl -fsSL https://get.mitos.run | sh

# See what the script would download for this machine, without downloading:
MITOS_SELF_TEST=1 sh install.sh
```

## From the first tagged release: manual binary

Download the archive and the checksums file for your platform from the
[releases page](https://github.com/mitos-run/mitos/releases), verify, then extract:

```bash
VER=v0.11.0           # the release tag
OS=linux              # or darwin
ARCH=amd64            # or arm64
BASE="https://github.com/mitos-run/mitos/releases/download/${VER}"

curl -fsSLO "${BASE}/mitos_${VER#v}_${OS}_${ARCH}.tar.gz"
curl -fsSLO "${BASE}/mitos_${VER#v}_checksums.txt"

# Verify (linux: sha256sum; macOS: shasum -a 256):
sha256sum --ignore-missing -c "mitos_${VER#v}_checksums.txt"

tar -xzf "mitos_${VER#v}_${OS}_${ARCH}.tar.gz" mitos
sudo install -m 0755 mitos /usr/local/bin/mitos
```

Windows users download the `_windows_<arch>.zip` archive and place `mitos.exe` on
their `PATH`.

## Coming with releases: package managers

These paths are configured in `.goreleaser.yaml` and produced by a tagged release.
They are listed here so the docs match the config, but they only work once the
release publishes them. Do not assume they work before then.

- Homebrew (macOS and linuxbrew):

  ```bash
  brew install mitos-run/tap/mitos
  ```

- Debian/Ubuntu (`.deb`) and Fedora/RHEL (`.rpm`): download the package for your
  architecture from the releases page and install with `dpkg -i` or `rpm -i`.

- Windows: scoop and winget manifests are planned; until they publish, use the
  manual `.zip` archive.

## Verifying a download

Every release publishes `mitos_<version>_checksums.txt`, a SHA256 manifest over all
archives. The install script verifies automatically. To verify by hand:

```bash
# Linux:
sha256sum --ignore-missing -c mitos_<version>_checksums.txt
# macOS:
shasum -a 256 -c mitos_<version>_checksums.txt --ignore-missing
```
