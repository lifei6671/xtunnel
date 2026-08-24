#!/bin/sh
set -eu

# 本脚本只安装仓库锁定的 OpenAPI Validator，失败时保留已有工具。
script_dir=$(CDPATH= cd "$(dirname "$0")" && pwd)
repo_root=$(CDPATH= cd "$script_dir/.." && pwd)
versions_file="$script_dir/versions.env"
bin_dir="$repo_root/.tools/bin"
vacuum_bin="$bin_dir/vacuum"
archive_temp=''
extract_temp_dir=''

fail() {
    printf 'openapi bootstrap: %s\n' "$1" >&2
    exit 1
}

cleanup() {
    if [ -n "$archive_temp" ]; then
        rm -f -- "$archive_temp"
    fi
    if [ -n "$extract_temp_dir" ]; then
        rm -rf -- "$extract_temp_dir"
    fi
}

trap cleanup 0
trap 'cleanup; exit 1' HUP INT TERM

[ -r "$versions_file" ] || fail "missing $versions_file"
. "$versions_file"

select_distribution() {
    platform=$(uname -s)
    architecture=$(uname -m)

    case "$platform:$architecture" in
        Linux:x86_64)
            vacuum_asset=$VACUUM_LINUX_AMD64_ASSET
            vacuum_archive_sha256=$VACUUM_LINUX_AMD64_ARCHIVE_SHA256
            vacuum_binary_sha256=$VACUUM_LINUX_AMD64_BINARY_SHA256
            ;;
        Linux:aarch64|Linux:arm64)
            vacuum_asset=$VACUUM_LINUX_ARM64_ASSET
            vacuum_archive_sha256=$VACUUM_LINUX_ARM64_ARCHIVE_SHA256
            vacuum_binary_sha256=$VACUUM_LINUX_ARM64_BINARY_SHA256
            ;;
        *)
            fail "unsupported platform $platform/$architecture; use Linux x86_64 or arm64"
            ;;
    esac
}

file_sha256() {
    checksum=$(sha256sum "$1")
    printf '%s\n' "${checksum%% *}"
}

vacuum_is_current() {
    [ -x "$vacuum_bin" ] || return 1
    [ "$(file_sha256 "$vacuum_bin")" = "$vacuum_binary_sha256" ] || return 1
    [ "$("$vacuum_bin" version 2>/dev/null)" = "$VACUUM_VERSION" ] || return 1
}

select_distribution

command -v curl >/dev/null 2>&1 || fail 'curl is required'
command -v sha256sum >/dev/null 2>&1 || fail 'sha256sum is required'
command -v mktemp >/dev/null 2>&1 || fail 'mktemp is required'
command -v tar >/dev/null 2>&1 || fail 'tar is required'

umask 077
mkdir -p "$bin_dir"

if vacuum_is_current; then
    printf 'vacuum %s is already installed.\n' "$VACUUM_VERSION"
else
    archive_temp=$(mktemp "$bin_dir/.vacuum-archive.XXXXXX") || \
        fail 'cannot create temporary vacuum archive'
    extract_temp_dir=$(mktemp -d "$bin_dir/.vacuum-extract.XXXXXX") || \
        fail 'cannot create temporary vacuum directory'
    vacuum_url="$VACUUM_DOWNLOAD_BASE_URL/$vacuum_asset"

    printf 'Downloading vacuum %s for %s...\n' "$VACUUM_VERSION" "$architecture"
    curl --fail --location --silent --show-error --output "$archive_temp" "$vacuum_url" || \
        fail 'vacuum download failed'

    actual_archive_sha256=$(file_sha256 "$archive_temp")
    [ "$actual_archive_sha256" = "$vacuum_archive_sha256" ] || \
        fail "vacuum archive SHA-256 mismatch: got $actual_archive_sha256"

    tar -xzf "$archive_temp" -C "$extract_temp_dir" || fail 'vacuum archive extraction failed'
    extracted_vacuum="$extract_temp_dir/vacuum"
    [ -f "$extracted_vacuum" ] || fail 'vacuum archive does not contain the expected binary'

    actual_binary_sha256=$(file_sha256 "$extracted_vacuum")
    [ "$actual_binary_sha256" = "$vacuum_binary_sha256" ] || \
        fail "vacuum binary SHA-256 mismatch: got $actual_binary_sha256"
    chmod 0755 "$extracted_vacuum"
    [ "$("$extracted_vacuum" version)" = "$VACUUM_VERSION" ] || \
        fail 'vacuum version check failed'

    mv -f -- "$extracted_vacuum" "$vacuum_bin"
    printf 'Installed vacuum %s.\n' "$VACUUM_VERSION"
fi

printf 'OpenAPI toolchain bootstrap completed in %s.\n' "$bin_dir"
