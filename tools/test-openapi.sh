#!/bin/sh
set -eu

# 本脚本在隔离目录验证 Wrapper 的成功路径和关键失败分支，不改动真实契约。
script_dir=$(CDPATH= cd "$(dirname "$0")" && pwd)
repo_root=$(CDPATH= cd "$script_dir/.." && pwd)
managed_vacuum="$repo_root/.tools/bin/vacuum"
managed_oapi_codegen="$repo_root/.tools/bin/oapi-codegen"
typescript_cli="$repo_root/tools/openapi-ts/node_modules/openapi-typescript/bin/cli.js"
go_generated_file="$repo_root/internal/server/managementapi/contract.gen.go"
typescript_generated_file="$repo_root/web/src/api/schema.gen.ts"
typescript_client_file="$repo_root/web/src/api/client.ts"
test_root=''
go_generated_backup=''
typescript_generated_backup=''

fail() {
    printf 'openapi test: %s\n' "$1" >&2
    exit 1
}

cleanup() {
    if [ -n "$go_generated_backup" ] && [ -f "$go_generated_backup" ]; then
        cp -- "$go_generated_backup" "$go_generated_file"
    fi
    if [ -n "$typescript_generated_backup" ] && [ -f "$typescript_generated_backup" ]; then
        cp -- "$typescript_generated_backup" "$typescript_generated_file"
    fi
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
[ "${GOTOOLCHAIN-}" = 'local' ] || fail 'GOTOOLCHAIN must be set to local'
[ -x "$managed_vacuum" ] || fail "missing managed vacuum; run $script_dir/bootstrap-openapi.sh"
[ -x "$managed_oapi_codegen" ] || \
    fail "missing managed oapi-codegen; run $script_dir/bootstrap-openapi.sh"
[ -r "$typescript_cli" ] || \
    fail 'missing managed openapi-typescript; run npm --prefix tools/openapi-ts ci'
[ -r "$typescript_client_file" ] || fail "missing TypeScript API client: $typescript_client_file"

test_root=$(mktemp -d "$repo_root/.tools/openapi-test.XXXXXX") || \
    fail 'cannot create temporary test directory'

expect_status 0 canonical-from-other-cwd \
    sh -c 'cd "$1" && exec sh "$2" validate' sh "$test_root" "$script_dir/openapi.sh"
expect_status 0 canonical-breaking sh "$script_dir/openapi.sh" breaking
expect_status 2 missing-command sh "$script_dir/openapi.sh"
expect_status 2 unknown-command sh "$script_dir/openapi.sh" unknown
expect_status 2 extra-argument sh "$script_dir/openapi.sh" validate extra

isolated_root="$test_root/repository"
isolated_tools="$isolated_root/tools"
isolated_openapi="$isolated_root/api/openapi"
isolated_bin="$isolated_root/.tools/bin"
isolated_wrapper="$isolated_tools/openapi.sh"
isolated_spec="$isolated_openapi/openapi.yaml"
isolated_baseline="$isolated_openapi/openapi.v0.1.baseline.yaml"

mkdir -p "$isolated_tools" "$isolated_openapi" "$isolated_bin"
cp "$script_dir/bootstrap-openapi.sh" "$script_dir/openapi.sh" \
    "$script_dir/versions.env" "$isolated_tools/"
cp "$repo_root/api/openapi/ruleset.yaml" "$isolated_openapi/"
chmod 0755 "$isolated_wrapper"

# 命令解析必须先于工具预检，干净 Checkout 中未知子命令仍是用法错误。
expect_status 2 unknown-command-without-tool sh "$isolated_wrapper" unknown

cp "$managed_vacuum" "$isolated_bin/vacuum"
chmod 0755 "$isolated_bin/vacuum"

cat >"$isolated_baseline" <<'EOF'
openapi: 3.1.0
info:
  title: Breaking baseline fixture
  version: 0.1.0
  description: Contract used to prove that Vacuum detects a removed operation.
servers:
  - url: /api/v1
paths:
  /health:
    get:
      operationId: getHealth
      responses:
        "204":
          description: Healthy
EOF
cp "$isolated_baseline" "$isolated_spec"
expect_status 0 unchanged-contract sh "$isolated_wrapper" breaking

cat >"$isolated_spec" <<'EOF'
openapi: 3.1.0
info:
  title: Breaking baseline fixture
  version: 0.1.0
  description: Contract used to prove that Vacuum detects a removed operation.
servers:
  - url: /api/v1
paths: {}
EOF
expect_status 1 removed-operation-is-breaking sh "$isolated_wrapper" breaking
grep -F 'breaking-change' "$test_root/removed-operation-is-breaking.out" >/dev/null || \
    fail 'removed operation failed without a Vacuum breaking-change violation'

rm -f -- "$isolated_baseline"
expect_status 1 missing-breaking-baseline sh "$isolated_wrapper" breaking

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
    printf "VACUUM_LINUX_ARM64_ASSET='invalid-archive.tar.gz'\n"
} >>"$bootstrap_tools/versions.env"
expect_status 1 archive-checksum sh "$bootstrap_tools/bootstrap-openapi.sh"
grep -F 'vacuum archive SHA-256 mismatch' "$test_root/archive-checksum.out" >/dev/null || \
    fail 'archive checksum case failed for an unexpected reason'
[ "$(cat "$bootstrap_bin/vacuum")" = 'existing vacuum' ] || \
    fail 'failed bootstrap replaced the existing vacuum binary'

# Generator 构建失败也必须保留已有受管二进制，不能把半成品发布到工具目录。
generator_bootstrap_root="$test_root/generator-bootstrap-repository"
generator_bootstrap_tools="$generator_bootstrap_root/tools"
generator_bootstrap_bin="$generator_bootstrap_root/.tools/bin"
fake_go_bin="$test_root/fake-go-bin"
mkdir -p "$generator_bootstrap_tools" "$generator_bootstrap_bin" "$fake_go_bin"
cp "$script_dir/bootstrap-openapi.sh" "$script_dir/check-go-version.sh" \
    "$script_dir/versions.env" "$generator_bootstrap_tools/"
cp "$managed_vacuum" "$generator_bootstrap_bin/vacuum"
cat >"$generator_bootstrap_bin/oapi-codegen" <<'EOF'
#!/bin/sh
printf 'existing generator\n'
EOF
cp "$generator_bootstrap_bin/oapi-codegen" "$test_root/existing-oapi-codegen"
cat >"$fake_go_bin/go" <<'EOF'
#!/bin/sh
case "${1-}:${2-}" in
    env:GOVERSION)
        printf 'go1.27.0\n'
        ;;
    env:GOTOOLCHAIN)
        printf 'local\n'
        ;;
    *)
        exit 1
        ;;
esac
EOF
chmod 0755 "$generator_bootstrap_tools/bootstrap-openapi.sh" \
    "$generator_bootstrap_tools/check-go-version.sh" \
    "$generator_bootstrap_bin/vacuum" "$generator_bootstrap_bin/oapi-codegen" \
    "$fake_go_bin/go"
expect_status 1 generator-build-failure env GOTOOLCHAIN=local \
    PATH="$fake_go_bin:$PATH" sh "$generator_bootstrap_tools/bootstrap-openapi.sh"
grep -F 'oapi-codegen build failed' "$test_root/generator-build-failure.out" >/dev/null || \
    fail 'generator build case failed for an unexpected reason'
cmp -s "$test_root/existing-oapi-codegen" "$generator_bootstrap_bin/oapi-codegen" || \
    fail 'failed generator build replaced the existing oapi-codegen binary'

# 生成检查必须真实拒绝 Go/TypeScript 任意一端的漂移，并在退出时恢复原始产物。
expect_status 0 canonical-generated-contract sh "$script_dir/openapi.sh" generate-check
go_generated_backup="$test_root/contract.gen.go"
typescript_generated_backup="$test_root/schema.gen.ts"
cp "$go_generated_file" "$go_generated_backup"
cp "$typescript_generated_file" "$typescript_generated_backup"

printf '\n// drift\n' >>"$go_generated_file"
expect_status 1 go-generated-drift sh "$script_dir/openapi.sh" generate-check
cp "$go_generated_backup" "$go_generated_file"

printf '\n// drift\n' >>"$typescript_generated_file"
expect_status 1 typescript-generated-drift sh "$script_dir/openapi.sh" generate-check
cp "$typescript_generated_backup" "$typescript_generated_file"

rm -f -- "$go_generated_file"
expect_status 1 missing-go-generated-contract sh "$script_dir/openapi.sh" generate-check
cp "$go_generated_backup" "$go_generated_file"

# 字节一致只能证明可重复；这些断言额外锁定 M5 后续 Handler/Web 依赖的传输语义。
strict_operations=$(
    sed -n '/^type StrictServerInterface interface {$/,/^}$/p' "$go_generated_file" |
        grep -c 'ResponseObject, error)'
)
[ "$strict_operations" -eq 26 ] || \
    fail "generated Go strict interface has $strict_operations operations, want 26"
typescript_operations=$(grep -c 'operations\["' "$typescript_generated_file")
[ "$typescript_operations" -eq 26 ] || \
    fail "generated TypeScript paths have $typescript_operations operations, want 26"
grep -F 'Health   nullable.Nullable[HealthCheckInput]' "$go_generated_file" >/dev/null || \
    fail 'generated Go PATCH contract lost nullable health state'
grep -F 'Exposure nullable.Nullable[ExposurePatch]' "$go_generated_file" >/dev/null || \
    fail 'generated Go PATCH contract lost nullable exposure state'
grep -F 'type StrictServerInterface interface {' "$go_generated_file" >/dev/null || \
    fail 'generated Go strict server interface is missing'
grep -F 'Body          io.Reader' "$go_generated_file" >/dev/null || \
    fail 'generated Go audit export contract is not streaming'
grep -F 'readonly "application/x-ndjson": string;' "$typescript_generated_file" >/dev/null || \
    fail 'generated TypeScript audit export media type is missing'
grep -F 'type UpdateServiceApplicationMergePatchPlusJSONRequestBody' "$go_generated_file" >/dev/null || \
    fail 'generated Go contract lost merge-patch media type'
grep -F 'ETag *string' "$go_generated_file" >/dev/null || \
    fail 'generated Go contract lost ETag response headers'
grep -F 'CacheControl *string' "$go_generated_file" >/dev/null || \
    fail 'generated Go contract lost Cache-Control response headers'
grep -F 'readonly scheme: "http";' "$typescript_generated_file" >/dev/null || \
    fail 'generated TypeScript HTTP discriminator differs from the Wire value'
grep -F 'readonly type: "TCP";' "$typescript_generated_file" >/dev/null || \
    fail 'generated TypeScript TCP health discriminator differs from the Wire value'
grep -F 'readonly password: $Write<string>;' "$typescript_generated_file" >/dev/null || \
    fail 'generated TypeScript contract lost writeOnly password semantics'
grep -F 'readonly "application/merge-patch+json"' "$typescript_generated_file" >/dev/null || \
    fail 'generated TypeScript contract lost merge-patch media type'
grep -F 'readonly ETag:' "$typescript_generated_file" >/dev/null || \
    fail 'generated TypeScript contract lost ETag response headers'
grep -F 'readonly "Cache-Control":' "$typescript_generated_file" >/dev/null || \
    fail 'generated TypeScript contract lost Cache-Control response headers'
grep -F 'baseUrl: "/api/v1"' "$typescript_client_file" >/dev/null || \
    fail 'TypeScript API client base URL differs from the OpenAPI server base path'
grep -F 'credentials: "same-origin"' "$typescript_client_file" >/dev/null || \
    fail 'TypeScript API client lost same-origin cookie credentials'

printf 'OpenAPI validation and generation tests passed.\n'
