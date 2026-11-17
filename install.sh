#!/usr/bin/env sh
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# AX CLI one-line installer for macOS and Linux.
#
# Usage:
#   curl -fsSL https://github.com/google/ax/releases/latest/download/install.sh | sh
#
# Environment overrides:
#   AX_INSTALL_DIR          Target directory for `ax` and `ax-studio` (default: ~/.ax/bin)
#   AX_GITHUB_REPO          GitHub org/repo for AX CLI releases (default: google/ax)
#   AX_VERSION              AX CLI release tag (e.g. v0.1.0); empty uses latest release
#   AX_RELEASE_BASE_URL     Explicit base URL hosting ax_<os>_<arch>.tar.gz and checksums.txt
#   AX_INSTALL_LOCAL_BIN    Install `ax` from a local prebuilt binary (for CI / offline testing)
#   AX_STUDIO_GITHUB_REPO   GitHub org/repo for AX Studio releases (default: agentexecutor/studio)
#   AX_STUDIO_VERSION       AX Studio release tag (default: PINNED_STUDIO_VERSION; never floats to latest)
#   AX_STUDIO_SHA256        Optional SHA-256 digest override for custom AX_STUDIO_VERSION / AX_STUDIO_BASE_URL
#   AX_STUDIO_BASE_URL      Explicit base URL hosting ax-studio_<os>_<arch>.tar.gz and checksums.txt
#   AX_STUDIO_LOCAL_BIN     Install `ax-studio` from a local prebuilt binary
#   AX_SKIP_STUDIO          Set to "1" to skip installing the `ax-studio` web UI binary

set -eu

# Pinned AX Studio release matching this AX version's v1alpha1 gRPC API.
# TODO(studio): Populate PINNED_STUDIO_VERSION and pinned_studio_sha256() once
# agentexecutor/studio publishes its first release, and update them together
# when cutting an AX release (see docs/releasing.md).
PINNED_STUDIO_VERSION=""

AX_GITHUB_REPO="${AX_GITHUB_REPO:-google/ax}"
AX_VERSION="${AX_VERSION:-}"
AX_RELEASE_BASE_URL="${AX_RELEASE_BASE_URL:-}"
AX_INSTALL_LOCAL_BIN="${AX_INSTALL_LOCAL_BIN:-}"
AX_STUDIO_GITHUB_REPO="${AX_STUDIO_GITHUB_REPO:-agentexecutor/studio}"
AX_STUDIO_VERSION="${AX_STUDIO_VERSION:-$PINNED_STUDIO_VERSION}"
AX_STUDIO_SHA256="${AX_STUDIO_SHA256:-}"
AX_STUDIO_BASE_URL="${AX_STUDIO_BASE_URL:-}"
AX_STUDIO_LOCAL_BIN="${AX_STUDIO_LOCAL_BIN:-}"
AX_SKIP_STUDIO="${AX_SKIP_STUDIO:-0}"

# Official SHA-256 digests of ax-studio_<os>_<arch>.tar.gz for PINNED_STUDIO_VERSION.
pinned_studio_sha256() {
    case "${1}/${2}" in
        linux/amd64)  printf '%s' "" ;;
        linux/arm64)  printf '%s' "" ;;
        darwin/amd64) printf '%s' "" ;;
        darwin/arm64) printf '%s' "" ;;
        *)            printf '%s' "" ;;
    esac
}

detect_os() {
    _kernel="$(uname -s)"
    case "$_kernel" in
        Darwin) OS="darwin" ;;
        Linux)  OS="linux" ;;
        *)
            echo "Error: unsupported operating system '$_kernel'. AX supports macOS (Darwin) and Linux." >&2
            return 1
            ;;
    esac
}

detect_arch() {
    _machine="$(uname -m)"
    case "$_machine" in
        x86_64|amd64)   ARCH="amd64" ;;
        arm64|aarch64)  ARCH="arm64" ;;
        *)
            echo "Error: unsupported CPU architecture '$_machine'. AX supports amd64 (x86_64) and arm64." >&2
            return 1
            ;;
    esac
}

sha256_file() {
    _file="$1"
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$_file" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$_file" | awk '{print $1}'
    elif command -v openssl >/dev/null 2>&1; then
        openssl dgst -sha256 "$_file" | awk '{print $NF}'
    else
        echo "Error: sha256sum, shasum, or openssl is required to verify downloads." >&2
        return 1
    fi
}

preflight_os() {
    if [ "$OS" = "darwin" ]; then
        if command -v sw_vers >/dev/null 2>&1; then
            _macos_ver="$(sw_vers -productVersion)"
            _macos_major="$(printf '%s' "$_macos_ver" | cut -d. -f1)"
            if [ "${_macos_major:-0}" -lt 13 ]; then
                echo "Error: AX on macOS requires macOS 13 (Ventura) or later (detected $_macos_ver)." >&2
                return 1
            fi
        fi
    fi
}

resolve_install_dir() {
    if [ -n "${AX_INSTALL_DIR:-}" ]; then
        INSTALL_DIR="$AX_INSTALL_DIR"
    elif [ -n "${AX_HOME:-}" ]; then
        INSTALL_DIR="${AX_HOME}/bin"
    else
        INSTALL_DIR="${HOME}/.ax/bin"
    fi
    mkdir -p "$INSTALL_DIR"
}

verify_checksum() {
    _archive="$1"
    _archive_name="$2"
    _checksums_file="$3"

    _expected="$(awk -v target="$_archive_name" '$2 == target || $2 == "*" target { print $1; exit }' "$_checksums_file")"
    if [ -z "$_expected" ]; then
        echo "Error: checksum entry for '$_archive_name' not found in checksums.txt." >&2
        return 1
    fi
    _actual="$(sha256_file "$_archive")"
    if [ "$_expected" != "$_actual" ]; then
        echo "Error: SHA-256 mismatch for $_archive_name (expected $_expected, got $_actual)." >&2
        return 1
    fi
}

install_main() {
    detect_os
    detect_arch
    preflight_os
    resolve_install_dir

    _dest="${INSTALL_DIR}/ax"

    if [ -n "$AX_INSTALL_LOCAL_BIN" ]; then
        if [ ! -f "$AX_INSTALL_LOCAL_BIN" ]; then
            echo "Error: AX_INSTALL_LOCAL_BIN '$AX_INSTALL_LOCAL_BIN' does not exist." >&2
            return 1
        fi
        echo "==> Installing AX (${OS}/${ARCH}) from local binary to ${_dest}..."
        cp "$AX_INSTALL_LOCAL_BIN" "${_dest}.tmp"
        chmod 0755 "${_dest}.tmp"
        mv "${_dest}.tmp" "$_dest"
    else
        if ! command -v curl >/dev/null 2>&1; then
            echo "Error: curl is required to download AX." >&2
            return 1
        fi
        if ! command -v tar >/dev/null 2>&1; then
            echo "Error: tar is required to extract AX." >&2
            return 1
        fi

        if [ -n "$AX_RELEASE_BASE_URL" ]; then
            _base="${AX_RELEASE_BASE_URL%/}"
        elif [ -n "$AX_VERSION" ]; then
            _base="https://github.com/${AX_GITHUB_REPO}/releases/download/${AX_VERSION}"
        else
            _base="https://github.com/${AX_GITHUB_REPO}/releases/latest/download"
        fi

        _asset="ax_${OS}_${ARCH}.tar.gz"
        _tmpdir="$(mktemp -d "${TMPDIR:-/tmp}/ax-install.XXXXXX")"
        trap 'rm -rf "$_tmpdir"' EXIT INT TERM

        echo "==> Downloading AX (${OS}/${ARCH}) from ${_base}/${_asset}..."
        curl -fsSL "${_base}/${_asset}" -o "${_tmpdir}/${_asset}"
        curl -fsSL "${_base}/checksums.txt" -o "${_tmpdir}/checksums.txt"

        echo "==> Verifying SHA-256 checksum..."
        verify_checksum "${_tmpdir}/${_asset}" "$_asset" "${_tmpdir}/checksums.txt"

        tar -xzf "${_tmpdir}/${_asset}" -C "$_tmpdir"
        if [ ! -f "${_tmpdir}/ax" ]; then
            echo "Error: archive ${_asset} did not contain an 'ax' binary." >&2
            return 1
        fi
        chmod 0755 "${_tmpdir}/ax"
        mv "${_tmpdir}/ax" "$_dest"
    fi

    if [ "$OS" = "darwin" ] && command -v xattr >/dev/null 2>&1; then
        xattr -d com.apple.quarantine "$_dest" >/dev/null 2>&1 || true
    fi

    _ver="$("$_dest" version 2>/dev/null || echo "ax installed")"
    echo "==> Installed ${_ver} at ${_dest}"

    install_studio

    case ":${PATH}:" in
        *":${INSTALL_DIR}:"*)
            _on_path=1
            ;;
        *)
            _on_path=0
            ;;
    esac

    echo ""
    if [ "$_on_path" -eq 0 ]; then
        echo "Add ${INSTALL_DIR} to your PATH:"
        echo "  export PATH=\"${INSTALL_DIR}:\$PATH\""
        echo ""
    fi
    if [ "$OS" = "darwin" ]; then
        if ! (command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1) && \
           ! (command -v podman >/dev/null 2>&1 && podman info >/dev/null 2>&1); then
            echo "Prerequisite for local mode on macOS (skip if connecting to a remote cluster):"
            echo "  Start OrbStack / Docker Desktop (>= 6 GiB RAM), or set up Colima:"
            echo "    brew install docker colima"
            echo "    colima start --cpu 4 --memory 8 --disk 60 --vm-type vz --mount-type virtiofs"
            echo ""
        fi
    fi
    echo "Next step — launch AX Studio ('ax studio'), connect to a cluster ('ax ctx'), or start a local sandbox:"
    if [ "$_on_path" -eq 1 ]; then
        echo "  ax local start --server-image <server-img>"
    else
        echo "  ${_dest} local start --server-image <server-img>"
    fi
}

install_studio() {
    if [ "$AX_SKIP_STUDIO" = "1" ]; then
        return 0
    fi
    _studio_dest="${INSTALL_DIR}/ax-studio"

    if [ -n "$AX_STUDIO_LOCAL_BIN" ]; then
        if [ ! -f "$AX_STUDIO_LOCAL_BIN" ]; then
            echo "Warning: AX_STUDIO_LOCAL_BIN '$AX_STUDIO_LOCAL_BIN' does not exist; skipping ax-studio." >&2
            return 0
        fi
        cp "$AX_STUDIO_LOCAL_BIN" "${_studio_dest}.tmp"
        chmod 0755 "${_studio_dest}.tmp"
        mv "${_studio_dest}.tmp" "$_studio_dest"
    else
        command -v curl >/dev/null 2>&1 || return 0
        command -v tar >/dev/null 2>&1 || return 0

        if [ -n "$AX_STUDIO_BASE_URL" ]; then
            _sbase="${AX_STUDIO_BASE_URL%/}"
        elif [ -n "$AX_STUDIO_VERSION" ]; then
            _sbase="https://github.com/${AX_STUDIO_GITHUB_REPO}/releases/download/${AX_STUDIO_VERSION}"
        else
            # No studio version pinned yet and no explicit override set;
            # never float to releases/latest because ax-studio must match AX's v1alpha1 API.
            return 0
        fi

        _expected_sha="$(printf '%s' "$AX_STUDIO_SHA256" | tr '[:upper:]' '[:lower:]')"
        if [ -z "$_expected_sha" ] && [ -z "$AX_STUDIO_BASE_URL" ] && \
           [ "$AX_STUDIO_GITHUB_REPO" = "agentexecutor/studio" ] && \
           [ "$AX_STUDIO_VERSION" = "$PINNED_STUDIO_VERSION" ]; then
            _expected_sha="$(pinned_studio_sha256 "$OS" "$ARCH")"
            if [ -z "$_expected_sha" ]; then
                echo "Error: no pinned SHA-256 configured for ax-studio ${PINNED_STUDIO_VERSION} (${OS}/${ARCH})." >&2
                return 1
            fi
        fi

        _sasset="ax-studio_${OS}_${ARCH}.tar.gz"
        _stmp="$(mktemp -d "${TMPDIR:-/tmp}/ax-studio-install.XXXXXX")"
        if ! curl -fsSL "${_sbase}/${_sasset}" -o "${_stmp}/${_sasset}" 2>/dev/null; then
            rm -rf "$_stmp"
            return 0
        fi
        if [ -n "$_expected_sha" ]; then
            _actual_sha="$(sha256_file "${_stmp}/${_sasset}")"
            if [ "$_expected_sha" != "$_actual_sha" ]; then
                echo "Error: SHA-256 mismatch for ${_sasset} (expected ${_expected_sha}, got ${_actual_sha})." >&2
                rm -rf "$_stmp"
                return 1
            fi
        else
            if ! curl -fsSL "${_sbase}/checksums.txt" -o "${_stmp}/checksums.txt" 2>/dev/null; then
                rm -rf "$_stmp"
                return 0
            fi
            if ! verify_checksum "${_stmp}/${_sasset}" "$_sasset" "${_stmp}/checksums.txt"; then
                rm -rf "$_stmp"
                return 1
            fi
        fi
        tar -xzf "${_stmp}/${_sasset}" -C "$_stmp"
        if [ ! -f "${_stmp}/ax-studio" ]; then
            rm -rf "$_stmp"
            return 1
        fi
        chmod 0755 "${_stmp}/ax-studio"
        mv "${_stmp}/ax-studio" "$_studio_dest"
        rm -rf "$_stmp"
    fi

    if [ "$OS" = "darwin" ] && command -v xattr >/dev/null 2>&1; then
        xattr -d com.apple.quarantine "$_studio_dest" >/dev/null 2>&1 || true
    fi
    echo "==> Installed ax-studio at ${_studio_dest}"
}

if [ "${__INSTALL_SH_SOURCED:-0}" != "1" ]; then
    install_main "$@"
fi
