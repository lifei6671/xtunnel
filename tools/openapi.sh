#!/bin/sh
set -eu

# 开发机和 CI 共用此入口，校验唯一的 OpenAPI 机器契约及不可变初始基线。
script_dir=$(CDPATH= cd "$(dirname "$0")" && pwd)
repo_root=$(CDPATH= cd "$script_dir/.." && pwd)
versions_file="$script_dir/versions.env"
vacuum_bin="$repo_root/.tools/bin/vacuum"
oapi_codegen_bin="$repo_root/.tools/bin/oapi-codegen"
openapi_dir="$repo_root/api/openapi"
openapi_file="$openapi_dir/openapi.yaml"
baseline_file="$openapi_dir/openapi.v0.1.baseline.yaml"
ruleset_file="$openapi_dir/ruleset.yaml"
oapi_codegen_config="$openapi_dir/generate/oapi-codegen.yaml"
typescript_tools_dir="$script_dir/openapi-ts"
typescript_package_file="$typescript_tools_dir/package.json"
typescript_cli="$typescript_tools_dir/node_modules/openapi-typescript/bin/cli.js"
go_generated_file="$repo_root/internal/server/managementapi/contract.gen.go"
typescript_generated_file="$repo_root/web/src/api/schema.gen.ts"
generation_temp_dir=''

fail() {
    printf 'openapi: %s\n' "$1" >&2
    exit 1
}

usage() {
    printf 'Usage: %s {validate|breaking|generate|generate-check}\n' "$0" >&2
    exit 2
}

cleanup() {
    if [ -n "$generation_temp_dir" ]; then
        rm -rf -- "$generation_temp_dir"
    fi
}

trap cleanup 0
trap 'cleanup; exit 1' HUP INT TERM

[ "$#" -eq 1 ] || usage
case "$1" in
    validate|breaking|generate|generate-check)
        command_name=$1
        ;;
    *)
        usage
        ;;
esac

[ -r "$versions_file" ] || fail "missing $versions_file"
. "$versions_file"

select_binary_sha256() {
    platform=$(uname -s)
    architecture=$(uname -m)

    case "$platform:$architecture" in
        Linux:x86_64)
            expected_vacuum_sha256=$VACUUM_LINUX_AMD64_BINARY_SHA256
            ;;
        Linux:aarch64|Linux:arm64)
            expected_vacuum_sha256=$VACUUM_LINUX_ARM64_BINARY_SHA256
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

validate_vacuum() {
    command -v sha256sum >/dev/null 2>&1 || fail 'sha256sum is required'
    [ -x "$vacuum_bin" ] || fail "missing managed vacuum; run $script_dir/bootstrap-openapi.sh"

    actual_vacuum_sha256=$(file_sha256 "$vacuum_bin")
    [ "$actual_vacuum_sha256" = "$expected_vacuum_sha256" ] || \
        fail "managed vacuum SHA-256 mismatch: got $actual_vacuum_sha256"
    [ "$("$vacuum_bin" version)" = "$VACUUM_VERSION" ] || \
        fail 'managed vacuum version mismatch'
}

select_node_runtime() {
    if command -v node >/dev/null 2>&1; then
        node_command=node
        node_path_mode=posix
    elif command -v node.exe >/dev/null 2>&1 && command -v wslpath >/dev/null 2>&1; then
        node_command=node.exe
        node_path_mode=windows
    else
        fail 'Node.js is required for TypeScript contract generation'
    fi
}

select_oapi_path_mode() {
    # WSL 可以同时调用 Windows Node 与 Windows Go；两者是否存在彼此独立，
    # 因此 Generator 和 TypeScript CLI 必须分别决定路径格式，不能共用 Node 的判断。
    if command -v go >/dev/null 2>&1; then
        oapi_path_mode=posix
    elif command -v go.exe >/dev/null 2>&1 && command -v wslpath >/dev/null 2>&1; then
        oapi_path_mode=windows
    else
        fail 'Go is required to determine the managed oapi-codegen path mode'
    fi
}

node_path() {
    if [ "$node_path_mode" = windows ]; then
        wslpath -w "$1"
    else
        printf '%s\n' "$1"
    fi
}

oapi_path() {
    if [ "$oapi_path_mode" = windows ]; then
        wslpath -w "$1"
    else
        printf '%s\n' "$1"
    fi
}

validate_generators() {
    [ -x "$oapi_codegen_bin" ] || \
        fail "missing managed oapi-codegen; run $script_dir/bootstrap-openapi.sh"
    expected_oapi_version=$(printf '%s\nv%s' "$OAPI_CODEGEN_MODULE" "$OAPI_CODEGEN_VERSION")
    [ "$("$oapi_codegen_bin" -version 2>/dev/null)" = "$expected_oapi_version" ] || \
        fail 'managed oapi-codegen version mismatch'
    select_oapi_path_mode

    [ -r "$typescript_package_file" ] || fail "missing $typescript_package_file"
    [ -r "$typescript_cli" ] || \
        fail "missing managed openapi-typescript; run npm --prefix tools/openapi-ts ci"
    select_node_runtime
    cli_path=$(node_path "$typescript_cli")
    package_path=$(node_path "$typescript_package_file")
    expected_typescript_version=$("$node_command" -e \
        'const fs=require("fs");const p=JSON.parse(fs.readFileSync(process.argv[1],"utf8"));process.stdout.write(p.devDependencies["openapi-typescript"]);' \
        "$package_path")
    [ "$("$node_command" "$cli_path" --version)" = "v$expected_typescript_version" ] || \
        fail 'managed openapi-typescript version mismatch'
}

run_typescript_generator() {
    output_file=$1
    cli_path=$(node_path "$typescript_cli")
    spec_path=$(node_path "$openapi_file")
    output_path=$(node_path "$output_file")
    "$node_command" "$cli_path" "$spec_path" \
        --output "$output_path" \
        --immutable \
        --read-write-markers
}

generate_contracts() {
    output_dir=$1
    go_output="$output_dir/contract.gen.go"
    typescript_output="$output_dir/schema.gen.ts"
    go_config_path=$(oapi_path "$oapi_codegen_config")
    go_output_path=$(oapi_path "$go_output")
    go_spec_path=$(oapi_path "$openapi_file")

    "$oapi_codegen_bin" -config "$go_config_path" \
        -o "$go_output_path" "$go_spec_path"
    run_typescript_generator "$typescript_output"
}

prepare_generation() {
    command -v mktemp >/dev/null 2>&1 || fail 'mktemp is required'
    command -v cmp >/dev/null 2>&1 || fail 'cmp is required'
    [ -r "$oapi_codegen_config" ] || fail "missing $oapi_codegen_config"
    generation_temp_dir=$(mktemp -d "$repo_root/.tools/openapi-generate.XXXXXX") || \
        fail 'cannot create temporary OpenAPI generation directory'
    generate_contracts "$generation_temp_dir"
}

[ -r "$openapi_file" ] || fail "missing $openapi_file"

cd "$repo_root"
# Vacuum 会区分解析错误和规则失败；Wrapper 对外统一为“契约校验失败=1”，
# 从而与自身的命令用法错误（退出码 2）保持清晰边界。
case "$command_name" in
    validate)
        select_binary_sha256
        validate_vacuum
        [ -r "$ruleset_file" ] || fail "missing $ruleset_file"
        if ! VACUUM_NO_UPDATE_CHECK=true "$vacuum_bin" lint \
            --no-update-check \
            --no-style \
            --fail-severity error \
            --remote=false \
            --base "$openapi_dir" \
            --ruleset "$ruleset_file" \
            "$openapi_file"; then
            fail 'OpenAPI validation failed'
        fi
        ;;
    breaking)
        select_binary_sha256
        validate_vacuum
        [ -r "$ruleset_file" ] || fail "missing $ruleset_file"
        # 首个完整 REST Contract 没有历史前代；该独立文件是后续变更的固定比较起点。
        # 禁止把当前 Schema 路径传给 --original，否则会用自比较伪造 Breaking 通过。
        [ -r "$baseline_file" ] || fail "missing immutable OpenAPI baseline: $baseline_file"
        if ! VACUUM_NO_UPDATE_CHECK=true "$vacuum_bin" lint \
            --no-update-check \
            --no-style \
            --fail-severity error \
            --remote=false \
            --base "$openapi_dir" \
            --ruleset "$ruleset_file" \
            --original "$baseline_file" \
            --error-on-breaking \
            "$openapi_file"; then
            fail 'OpenAPI breaking-change check failed'
        fi
        ;;
    generate)
        validate_generators
        prepare_generation
        mkdir -p "$(dirname "$go_generated_file")" "$(dirname "$typescript_generated_file")"
        mv -f -- "$generation_temp_dir/contract.gen.go" "$go_generated_file"
        mv -f -- "$generation_temp_dir/schema.gen.ts" "$typescript_generated_file"
        printf 'Generated OpenAPI contracts.\n'
        ;;
    generate-check)
        validate_generators
        prepare_generation
        [ -f "$go_generated_file" ] || fail "missing generated Go contract: $go_generated_file"
        [ -f "$typescript_generated_file" ] || \
            fail "missing generated TypeScript contract: $typescript_generated_file"
        cmp -s "$generation_temp_dir/contract.gen.go" "$go_generated_file" || \
            fail 'generated Go contract differs from the repository state'
        cmp -s "$generation_temp_dir/schema.gen.ts" "$typescript_generated_file" || \
            fail 'generated TypeScript contract differs from the repository state'
        printf 'Generated OpenAPI contracts match the repository state.\n'
        ;;
esac
