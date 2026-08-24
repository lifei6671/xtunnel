#!/bin/sh
set -eu

# 本脚本在隔离目录验证 Wrapper 的成功路径和关键失败分支，不改动真实契约。
script_dir=$(CDPATH= cd "$(dirname "$0")" && pwd)
repo_root=$(CDPATH= cd "$script_dir/.." && pwd)
managed_vacuum="$repo_root/.tools/bin/vacuum"
test_root=''

fail() {
    printf 'openapi test: %s\n' "$1" >&2
    exit 1
}

cleanup() {
    if [ -n "$test_root" ]; then
        rm -rf -- "$test_root"
    fi
}

expect_status() {
    expected_status=$1
    case_name=$2
    shift 2
    output_file="$test_root/$case_name.out"

    if "$@" >"$output_file" 2>&1; then
        actual_status=0
    else
        actual_status=$?
    fi

    if [ "$actual_status" -ne "$expected_status" ]; then
        printf 'openapi test: %s returned %s, expected %s\n' \
            "$case_name" "$actual_status" "$expected_status" >&2
        cat "$output_file" >&2
        exit 1
    fi
}

trap cleanup 0
trap 'cleanup; exit 1' HUP INT TERM

command -v mktemp >/dev/null 2>&1 || fail 'mktemp is required'
command -v cp >/dev/null 2>&1 || fail 'cp is required'
[ -x "$managed_vacuum" ] || fail "missing managed vacuum; run $script_dir/bootstrap-openapi.sh"

test_root=$(mktemp -d "$repo_root/.tools/openapi-test.XXXXXX") || \
    fail 'cannot create temporary test directory'

expect_status 0 canonical-from-other-cwd \
    sh -c 'cd "$1" && exec sh "$2" validate' sh "$test_root" "$script_dir/openapi.sh"
expect_status 2 missing-command sh "$script_dir/openapi.sh"
expect_status 2 unknown-command sh "$script_dir/openapi.sh" unknown
expect_status 2 extra-argument sh "$script_dir/openapi.sh" validate extra

isolated_root="$test_root/repository"
isolated_tools="$isolated_root/tools"
isolated_openapi="$isolated_root/api/openapi"
isolated_bin="$isolated_root/.tools/bin"
isolated_wrapper="$isolated_tools/openapi.sh"
isolated_spec="$isolated_openapi/openapi.yaml"

mkdir -p "$isolated_tools" "$isolated_openapi" "$isolated_bin"
cp "$script_dir/bootstrap-openapi.sh" "$script_dir/openapi.sh" \
    "$script_dir/versions.env" "$isolated_tools/"
cp "$repo_root/api/openapi/ruleset.yaml" "$isolated_openapi/"
chmod 0755 "$isolated_wrapper"

# 命令解析必须先于工具预检，干净 Checkout 中未知子命令仍是用法错误。
expect_status 2 unknown-command-without-tool sh "$isolated_wrapper" unknown

cp "$managed_vacuum" "$isolated_bin/vacuum"
chmod 0755 "$isolated_bin/vacuum"

cat >"$isolated_spec" <<'EOF'
openapi: 3.1.0
info:
  title: Placeholder fixture
  version: 0.1.0
servers:
  - url: https://api.example.com
paths: {}
EOF
expect_status 1 placeholder-server sh "$isolated_wrapper" validate

cat >"$isolated_spec" <<'EOF'
openapi: 3.0.3
info:
  title: Dialect drift fixture
  version: 0.1.0
servers:
  - url: /api/v1
paths: {}
EOF
expect_status 1 dialect-drift sh "$isolated_wrapper" validate

cat >"$isolated_spec" <<'EOF'
openapi: 3.1.0
info:
  title: Base path drift fixture
  version: 0.1.0
servers:
  - url: /other
paths: {}
EOF
expect_status 1 base-path-drift sh "$isolated_wrapper" validate

cat >"$isolated_spec" <<'EOF'
openapi: 3.1.0
info:
  title: Multiple servers fixture
  version: 0.1.0
servers:
  - url: /api/v1
  - url: /api/v1
paths: {}
EOF
expect_status 1 multiple-servers sh "$isolated_wrapper" validate

cat >"$isolated_spec" <<'EOF'
openapi: 3.1.0
info:
  title: Server variables fixture
  version: 0.1.0
servers:
  - url: /api/v1
    variables: {}
paths: {}
EOF
expect_status 1 server-variables sh "$isolated_wrapper" validate

cat >"$isolated_spec" <<'EOF'
openapi: [
EOF
expect_status 1 malformed-yaml sh "$isolated_wrapper" validate

cat >"$isolated_spec" <<'EOF'
openapi: 3.1.0
info:
  title: Missing reference fixture
  version: 0.1.0
paths:
  /missing:
    get:
      responses:
        "200":
          $ref: "#/components/responses/Missing"
EOF
expect_status 1 unresolved-reference sh "$isolated_wrapper" validate

# 工具篡改测试使用从未执行过的副本，避免 WSL/NTFS 暂时占用刚退出的可执行文件。
tool_root="$test_root/tool-repository"
tool_tools="$tool_root/tools"
tool_openapi="$tool_root/api/openapi"
tool_bin="$tool_root/.tools/bin"
tool_wrapper="$tool_tools/openapi.sh"
mkdir -p "$tool_tools" "$tool_openapi" "$tool_bin"
cp "$script_dir/openapi.sh" "$script_dir/versions.env" "$tool_tools/"
cp "$repo_root/api/openapi/openapi.yaml" "$repo_root/api/openapi/ruleset.yaml" \
    "$tool_openapi/"
cp "$managed_vacuum" "$tool_bin/vacuum"
chmod 0755 "$tool_wrapper" "$tool_bin/vacuum"
printf 'tampered\n' >>"$tool_bin/vacuum"
expect_status 1 tampered-tool sh "$tool_wrapper" validate

rm -f -- "$tool_bin/vacuum"
fake_bin="$test_root/fake-bin"
mkdir -p "$fake_bin"
cat >"$fake_bin/vacuum" <<'EOF'
#!/bin/sh
exit 0
EOF
chmod 0755 "$fake_bin/vacuum"
expect_status 1 no-path-fallback env PATH="$fake_bin:$PATH" sh "$tool_wrapper" validate

# 校验和失败时不能覆盖已存在的工具。单独使用未执行过二进制的隔离目录，
# 避免 WSL/NTFS 对刚结束的可执行文件仍保持短暂占用。
bootstrap_root="$test_root/bootstrap-repository"
bootstrap_tools="$bootstrap_root/tools"
bootstrap_bin="$bootstrap_root/.tools/bin"
mkdir -p "$bootstrap_tools" "$bootstrap_bin"
cp "$script_dir/bootstrap-openapi.sh" "$script_dir/versions.env" "$bootstrap_tools/"
printf 'existing vacuum\n' >"$bootstrap_bin/vacuum"
chmod 0755 "$bootstrap_tools/bootstrap-openapi.sh" "$bootstrap_bin/vacuum"
printf 'invalid archive\n' >"$test_root/invalid-archive.tar.gz"
{
    printf "VACUUM_DOWNLOAD_BASE_URL='file://%s'\n" "$test_root"
    printf "VACUUM_LINUX_AMD64_ASSET='invalid-archive.tar.gz'\n"
} >>"$bootstrap_tools/versions.env"
expect_status 1 archive-checksum sh "$bootstrap_tools/bootstrap-openapi.sh"
[ "$(cat "$bootstrap_bin/vacuum")" = 'existing vacuum' ] || \
    fail 'failed bootstrap replaced the existing vacuum binary'

printf 'OpenAPI validation tests passed.\n'
