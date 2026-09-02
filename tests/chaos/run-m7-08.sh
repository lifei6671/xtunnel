#!/bin/sh

# 运行 M7-08 Large Transfer/Privileged Network Chaos。所有网络修改只发生在本
# Runner 创建的独立 namespace；退出时精确删除 nftables table、qdisc 和 namespace。

set -eu

usage() {
    printf '%s\n' 'Usage: run-m7-08.sh [-m smoke|full] [-b prebuilt-directory] -o output-directory [-s seed]'
    printf '%s\n' '  -m  smoke runs bounded netem and Reset; full runs the release matrix.'
    printf '%s\n' '  -b  Directory containing bootstrap.test and manifest.txt.'
    printf '%s\n' '  -o  New directory that receives logs and artifact-sha256.txt.'
    printf '%s\n' '  -s  Positive decimal payload seed (default: 20260902).'
    printf '%s\n' '  -h  Show this help.'
}

fail() {
    printf 'm7-08 chaos: %s\n' "$1" >&2
    exit 1
}

require_command() {
    command -v "$1" >/dev/null 2>&1 || fail "required command not found: $1"
}

manifest_value() {
    manifest_key=$1
    sed -n "s/^${manifest_key}=//p" "$prebuilt_dir/manifest.txt"
}

verify_manifest_value() {
    manifest_key=$1
    expected_value=$2
    actual_value=$(manifest_value "$manifest_key")
    [ "$actual_value" = "$expected_value" ] || \
        fail "prebuilt manifest $manifest_key must equal $expected_value"
}

verify_file_hash() {
    verify_path=$1
    expected_hash=$2
    actual_hash=$(sha256sum "$verify_path")
    actual_hash=${actual_hash%% *}
    [ "$actual_hash" = "$expected_hash" ] || fail "SHA-256 mismatch: $verify_path"
}

clear_netem() {
    if [ -n "$netns_name" ]; then
        ip netns exec "$netns_name" tc qdisc del dev lo root >/dev/null 2>&1 || true
    fi
}

clear_reset_table() {
    if [ -n "$netns_name" ] && [ -n "$nft_table" ]; then
        ip netns exec "$netns_name" nft delete table inet "$nft_table" >/dev/null 2>&1 || true
    fi
}

cleanup() {
    cleanup_status=$?
    trap - 0
    trap '' HUP INT TERM
    if [ -n "$test_pid" ]; then
        kill "$test_pid" >/dev/null 2>&1 || true
        wait "$test_pid" >/dev/null 2>&1 || true
        test_pid=
    fi
    clear_reset_table
    clear_netem
    if [ -n "$netns_name" ]; then
        if ! ip netns delete "$netns_name"; then
            printf '%s\n' 'm7-08 chaos: failed to delete network namespace' >&2
            [ "$cleanup_status" -ne 0 ] || cleanup_status=1
        fi
        netns_name=
    fi
    if [ -n "$run_dir" ]; then
        if ! rm -f "$run_dir/bootstrap.test" "$run_dir/manifest.txt" \
            "$run_dir/profile-ready" "$run_dir/profile-release" \
            "$run_dir/reset-ready" "$run_dir/reset-observed" "$run_dir/reset-release"; then
            printf '%s\n' 'm7-08 chaos: failed to remove temporary files' >&2
            [ "$cleanup_status" -ne 0 ] || cleanup_status=1
        fi
        if ! rmdir "$run_dir"; then
            printf '%s\n' 'm7-08 chaos: failed to remove temporary directory' >&2
            [ "$cleanup_status" -ne 0 ] || cleanup_status=1
        fi
        run_dir=
    fi
    exit "$cleanup_status"
}

configure_profile() {
    configured_profile=$1
    clear_netem
    case $configured_profile in
        clean)
            ;;
        smoke)
            ip netns exec "$netns_name" tc qdisc replace dev lo root netem \
                delay 20ms 5ms rate 100mbit
            ;;
        loss-1)
            ip netns exec "$netns_name" tc qdisc replace dev lo root netem loss 1%
            ;;
        loss-5)
            ip netns exec "$netns_name" tc qdisc replace dev lo root netem loss 5%
            ;;
        jitter-50)
            ip netns exec "$netns_name" tc qdisc replace dev lo root netem delay 100ms 50ms
            ;;
        bandwidth-10mbit)
            ip netns exec "$netns_name" tc qdisc replace dev lo root netem rate 10mbit
            ;;
        *)
            fail "unsupported network profile: $configured_profile"
            ;;
    esac
}

run_transfer_case() {
    transfer_profile=$1
    test_name=$2
    output_name=$3
    timeout_value=$4
    configure_profile clean
    profile_ready="$run_dir/profile-ready"
    profile_release="$run_dir/profile-release"
    rm -f "$profile_ready" "$profile_release"
    ip netns exec "$netns_name" env \
        GOTOOLCHAIN=local \
        XTUNNEL_RUN_M7_NETWORK_CHAOS=1 \
        XTUNNEL_M7_NETWORK_SEED="$seed" \
        XTUNNEL_M7_PROFILE_READY_FILE="$profile_ready" \
        XTUNNEL_M7_PROFILE_RELEASE_FILE="$profile_release" \
        timeout "$timeout_value" "$test_binary" \
        -test.run "^TestM7PrivilegedNetworkChaos$/${test_name}$" \
        -test.count=1 -test.timeout="$timeout_value" -test.v \
        >"$output_dir/$output_name" 2>&1 &
    test_pid=$!
    wait_for_marker "$profile_ready" 'profile-ready marker' 30 "$output_dir/$output_name"
    configure_profile "$transfer_profile"
    printf 'M7-08 profile: %s\n' "$transfer_profile"
    ip netns exec "$netns_name" tc qdisc show dev lo
    : >"$profile_release"
    if ! wait "$test_pid"; then
        test_pid=
        cat "$output_dir/$output_name" >&2
        fail "network profile $transfer_profile failed"
    fi
    test_pid=
    cat "$output_dir/$output_name"
}

wait_for_marker() {
    marker_path=$1
    marker_label=$2
    marker_timeout=$3
    marker_log=$4
    marker_elapsed=0
    while [ ! -s "$marker_path" ]; do
        if ! kill -0 "$test_pid" >/dev/null 2>&1; then
            wait "$test_pid" || true
            test_pid=
            cat "$marker_log" >&2
            fail "test exited before $marker_label"
        fi
        [ "$marker_elapsed" -lt "$marker_timeout" ] || fail "timed out waiting for $marker_label"
        sleep 1
        marker_elapsed=$((marker_elapsed + 1))
    done
}

run_reset_case() {
    configure_profile clean
    reset_ready="$run_dir/reset-ready"
    reset_observed="$run_dir/reset-observed"
    reset_release="$run_dir/reset-release"
    rm -f "$reset_ready" "$reset_observed" "$reset_release"

    ip netns exec "$netns_name" env \
        GOTOOLCHAIN=local \
        XTUNNEL_RUN_M7_NETWORK_CHAOS=1 \
        XTUNNEL_M7_NETWORK_SEED="$seed" \
        XTUNNEL_M7_RESET_READY_FILE="$reset_ready" \
        XTUNNEL_M7_RESET_OBSERVED_FILE="$reset_observed" \
        XTUNNEL_M7_RESET_RELEASE_FILE="$reset_release" \
        timeout 3m "$test_binary" \
        -test.run '^TestM7PrivilegedNetworkChaos$/tcp_reset_and_recovery$' \
        -test.count=1 -test.timeout=3m -test.v \
        >"$output_dir/reset.log" 2>&1 &
    test_pid=$!
    wait_for_marker "$reset_ready" 'reset-ready marker' 30 "$output_dir/reset.log"
    reset_port=$(sed -n '1p' "$reset_ready")
    case $reset_port in
        ''|*[!0-9]*) fail "invalid reset port: $reset_port" ;;
    esac
    if [ "$reset_port" -lt 1 ] || [ "$reset_port" -gt 65535 ]; then
        fail "invalid reset port: $reset_port"
    fi

    ip netns exec "$netns_name" nft -f - <<EOF
add table inet $nft_table
add chain inet $nft_table output { type filter hook output priority 0; policy accept; }
add rule inet $nft_table output ip daddr 127.0.0.1 tcp dport $reset_port counter reject with tcp reset
add rule inet $nft_table output ip saddr 127.0.0.1 tcp sport $reset_port counter reject with tcp reset
EOF
    sleep 1
    socket_before=$(ip netns exec "$netns_name" ss -Htan state established \
        dst 127.0.0.1 dport = ":$reset_port")
    [ -n "$socket_before" ] || fail 'cannot identify the active public socket before TCP Reset'
    {
        printf '%s\n' 'nft_before_socket_destroy:'
        ip netns exec "$netns_name" nft list table inet "$nft_table"
        printf 'socket_destroy_target=127.0.0.1:%s\n' "$reset_port"
        printf 'socket_before=%s\n' "$socket_before"
    } >"$output_dir/reset-network.txt" 2>&1
    # loopback reject 规则先证明目标流量确实命中；销毁 socket 前必须移除规则，
    # 否则它也会截获 SOCK_DESTROY 生成、发给对端的 RST。
    clear_reset_table
    {
        printf '%s\n' 'nft_removed_before_socket_destroy=true'
        ip netns exec "$netns_name" ss -K dst 127.0.0.1 dport = ":$reset_port"
    } >>"$output_dir/reset-network.txt" 2>&1
    sleep 1
    socket_after=$(ip netns exec "$netns_name" ss -Htan state established \
        dst 127.0.0.1 dport = ":$reset_port")
    if [ -n "$socket_after" ]; then
        printf 'socket_after=%s\n' "$socket_after" >>"$output_dir/reset-network.txt"
        fail 'kernel did not destroy the active socket; SOCK_DESTROY support is required'
    fi
    wait_for_marker "$reset_observed" 'reset-observed marker' 30 "$output_dir/reset.log"
    printf 'runner_reset_observed=%s\n' "$(sed -n '1p' "$reset_observed")" \
        >>"$output_dir/reset-network.txt"
    : >"$reset_release"
    if ! wait "$test_pid"; then
        test_pid=
        cat "$output_dir/reset.log" >&2
        fail 'TCP Reset recovery failed'
    fi
    test_pid=
    cat "$output_dir/reset.log"
}

write_environment() {
    {
        printf 'mode=%s\n' "$mode"
        printf 'seed=%s\n' "$seed"
        printf 'commit=%s\n' "$(git rev-parse HEAD)"
        printf 'worktree_clean=%s\n' "$worktree_clean"
        printf 'namespace=%s\n' "$netns_name"
        printf 'uid=%s\n' "$(id -u)"
        printf 'go_version=%s\n' "$go_version"
        printf 'go_toolchain=%s\n' "$go_toolchain"
        printf 'uname=%s\n' "$(uname -a)"
        printf 'ip_version=%s\n' "$(ip -Version 2>&1)"
        printf 'tc_version=%s\n' "$(tc -Version 2>&1)"
        printf 'nft_version=%s\n' "$(nft --version 2>&1)"
        printf '%s\n' 'full_profiles=clean-1GiB,loss-1,loss-5,jitter-100ms-50ms,bandwidth-10mbit,tcp-reset'
        printf '%s\n' 'impaired_bytes_per_direction=8388608'
        if [ -n "$prebuilt_dir" ]; then
            printf 'prebuilt_manifest_sha256=%s\n' "$(sha256sum "$run_dir/manifest.txt" | sed 's/ .*//')"
        fi
    } >"$output_dir/environment.txt"
}

write_artifact_hashes() {
    (
        cd "$output_dir"
        for artifact_file in environment.txt reset-network.txt *.log; do
            sha256sum "$artifact_file"
        done
    ) >"$output_dir/artifact-sha256.txt"
}

mode=smoke
prebuilt_dir=
output_argument=
seed=20260902
run_dir=
test_binary=
test_pid=
netns_name=
nft_table=

while [ "$#" -gt 0 ]; do
    case $1 in
        -m)
            [ "$#" -ge 2 ] || fail '-m requires a value'
            mode=$2
            shift 2
            ;;
        -b)
            [ "$#" -ge 2 ] || fail '-b requires a value'
            prebuilt_dir=$2
            shift 2
            ;;
        -o)
            [ "$#" -ge 2 ] || fail '-o requires a value'
            output_argument=$2
            shift 2
            ;;
        -s)
            [ "$#" -ge 2 ] || fail '-s requires a value'
            seed=$2
            shift 2
            ;;
        -h)
            usage
            exit 0
            ;;
        --)
            shift
            break
            ;;
        *)
            usage >&2
            fail "unknown argument: $1"
            ;;
    esac
done

[ "$#" -eq 0 ] || fail 'positional arguments are not supported'
case $mode in
    smoke|full) ;;
    *) fail "unsupported mode: $mode" ;;
esac
[ -n "$output_argument" ] || fail '-o is required'
case $seed in
    ''|*[!0-9]*) fail 'seed must be a positive decimal integer' ;;
esac
[ "$seed" -gt 0 ] || fail 'seed must be greater than zero'

require_command uname
[ "$(uname -s)" = Linux ] || fail 'M7-08 runner requires Linux'
require_command id
[ "$(id -u)" -eq 0 ] || fail 'M7-08 runner requires root with CAP_NET_ADMIN'
for required_command in git ip tc nft ss env timeout mktemp mkdir rm rmdir sed sha256sum stat sleep; do
    require_command "$required_command"
done

script_dir=$(CDPATH='' cd -P "$(dirname "$0")" && pwd -P)
repo_root=$(CDPATH='' cd -P "$script_dir/../.." && pwd -P)
[ -f "$repo_root/go.mod" ] || fail 'repository root does not contain go.mod'
cd "$repo_root"

git_root=$(git rev-parse --show-toplevel) || fail 'cannot resolve Git repository root'
git_root=$(CDPATH='' cd -P "$git_root" && pwd -P)
[ "$git_root" = "$repo_root" ] || fail 'script is not running from the XTunnel repository root'

worktree_clean=true
if [ -n "$(git status --porcelain --untracked-files=all)" ]; then
    worktree_clean=false
fi
if [ "$mode" = full ] && [ "$worktree_clean" != true ]; then
    fail 'full mode requires a clean worktree'
fi
[ ! -e "$output_argument" ] || fail "output path already exists: $output_argument"
mkdir -p "$output_argument"
output_dir=$(CDPATH='' cd -P "$output_argument" && pwd -P)

if [ ! -d /tmp ] || [ ! -w /tmp ]; then
    fail 'a writable Linux-native /tmp is required'
fi
run_dir=$(mktemp -d /tmp/xtunnel-m7-08.XXXXXX) || fail 'cannot create temporary run directory'
run_filesystem=$(stat -f -c %T "$run_dir") || fail 'cannot identify temporary run filesystem'
case $run_filesystem in
    9p|drvfs|v9fs) fail 'temporary directory must not use WSL DrvFS' ;;
esac
trap 'cleanup' 0
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

if [ -n "$prebuilt_dir" ]; then
    require_command cp
    require_command chmod
    [ -d "$prebuilt_dir" ] || fail "prebuilt directory not found: $prebuilt_dir"
    prebuilt_dir=$(CDPATH='' cd -P "$prebuilt_dir" && pwd -P)
    [ -r "$prebuilt_dir/manifest.txt" ] || fail 'prebuilt manifest not found'
    [ -f "$prebuilt_dir/bootstrap.test" ] || fail 'prebuilt bootstrap.test not found'
    verify_manifest_value go_version go1.27.0
    verify_manifest_value toolchain local
    verify_manifest_value goos linux
    verify_manifest_value goarch amd64
    verify_manifest_value goamd64 v1
    verify_manifest_value cgo_enabled 0
    [ "$(manifest_value commit)" = "$(git rev-parse HEAD)" ] || \
        fail 'prebuilt manifest Commit does not match the current checkout'
    manifest_clean=$(manifest_value worktree_clean)
    case $manifest_clean in
        true|false) ;;
        *) fail 'prebuilt manifest worktree_clean must equal true or false' ;;
    esac
    [ "$mode" != full ] || verify_manifest_value worktree_clean true
    expected_binary_hash=$(manifest_value bootstrap_sha256)
    [ -n "$expected_binary_hash" ] || fail 'prebuilt manifest is missing bootstrap_sha256'
    go_version=$(manifest_value go_version)
    go_toolchain=$(manifest_value toolchain)
    verify_file_hash "$prebuilt_dir/bootstrap.test" "$expected_binary_hash"
    cp "$prebuilt_dir/manifest.txt" "$prebuilt_dir/bootstrap.test" "$run_dir/"
    chmod 400 "$run_dir/manifest.txt"
    chmod 500 "$run_dir/bootstrap.test"
    verify_file_hash "$run_dir/bootstrap.test" "$expected_binary_hash"
else
    require_command go
    require_command chmod
    [ -x "$repo_root/tools/check-go-version.sh" ] || \
        fail 'repository root does not contain an executable Go version check'
    export GOTOOLCHAIN=local
    "$repo_root/tools/check-go-version.sh"
    go_version=$(go env GOVERSION)
    go_toolchain=$(go env GOTOOLCHAIN)
    go test -c -o "$run_dir/bootstrap.test" ./internal/server/bootstrap
    chmod 500 "$run_dir/bootstrap.test"
fi
test_binary="$run_dir/bootstrap.test"

netns_candidate="xtunnel-m708-$$"
nft_table="xtunnel_m708_$$"
ip netns add "$netns_candidate"
netns_name=$netns_candidate
ip netns exec "$netns_name" ip link set lo up
ip netns exec "$netns_name" nft list ruleset >/dev/null
configure_profile clean
write_environment

printf 'M7-08 mode: %s\n' "$mode"
printf 'M7-08 commit: %s\n' "$(git rev-parse HEAD)"
printf 'M7-08 platform: %s\n' "$(uname -a)"
printf 'M7-08 namespace: %s\n' "$netns_name"
printf 'M7-08 seed: %s\n' "$seed"

case $mode in
    smoke)
        run_transfer_case smoke impaired_bidirectional_transfer smoke-netem.log 3m
        run_reset_case
        ;;
    full)
        run_transfer_case clean large_bidirectional_transfer large-1gib.log 12m
        run_transfer_case loss-1 impaired_bidirectional_transfer loss-1.log 5m
        run_transfer_case loss-5 impaired_bidirectional_transfer loss-5.log 5m
        run_transfer_case jitter-50 impaired_bidirectional_transfer jitter-50.log 5m
        run_transfer_case bandwidth-10mbit impaired_bidirectional_transfer bandwidth-10mbit.log 5m
        run_reset_case
        ;;
esac

clear_netem
write_artifact_hashes
(
    cd "$output_dir"
    sha256sum -c artifact-sha256.txt
)
printf 'M7-08 artifact directory: %s\n' "$output_dir"
printf '%s\n' 'M7-08 privileged network chaos completed.'
