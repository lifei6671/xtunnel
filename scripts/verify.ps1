[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$scriptDirectory = Split-Path -Parent $PSCommandPath
$repositoryRoot = Split-Path -Parent $scriptDirectory
$composeFile = Join-Path $repositoryRoot 'compose.codex.yml'
$originalLocation = Get-Location
$exitCode = 0

try {
    Set-Location $repositoryRoot

    & docker compose -f $composeFile up -d --pull always go-runner web-runner
    if ($LASTEXITCODE -ne 0) {
        $exitCode = $LASTEXITCODE
    }

    if ($exitCode -eq 0) {
        # 与仓库 toolchain、CI 和 OCI Builder 使用同一个精确补丁版本。
        $goVersion = (& docker compose -f $composeFile exec -T go-runner go env GOVERSION).Trim()
        if ($LASTEXITCODE -ne 0) {
            $exitCode = $LASTEXITCODE
        }
        elseif ($goVersion -ne 'go1.27.0') {
            throw "go-runner must use go1.27.0, got $goVersion"
        }
    }

    if ($exitCode -eq 0) {
        $goToolchain = (& docker compose -f $composeFile exec -T go-runner go env GOTOOLCHAIN).Trim()
        if ($LASTEXITCODE -ne 0) {
            $exitCode = $LASTEXITCODE
        }
        elseif ($goToolchain -ne 'local') {
            throw "go-runner GOTOOLCHAIN must be local, got $goToolchain"
        }
    }

    if ($exitCode -eq 0) {
        & docker compose -f $composeFile exec -T web-runner npm --prefix web ci
        if ($LASTEXITCODE -ne 0) { $exitCode = $LASTEXITCODE }
    }

    if ($exitCode -eq 0) {
        & docker compose -f $composeFile exec -T web-runner npm --prefix web run check
        if ($LASTEXITCODE -ne 0) { $exitCode = $LASTEXITCODE }
    }

    if ($exitCode -eq 0) {
        & docker compose -f $composeFile exec -T web-runner npm --prefix web run build
        if ($LASTEXITCODE -ne 0) { $exitCode = $LASTEXITCODE }
    }

    if ($exitCode -eq 0) {
        & docker compose -f $composeFile exec -T go-runner go test ./...
        if ($LASTEXITCODE -ne 0) {
            $exitCode = $LASTEXITCODE
        }
    }

    if ($exitCode -eq 0) {
        & docker compose -f $composeFile exec -T go-runner go vet ./...
        if ($LASTEXITCODE -ne 0) {
            $exitCode = $LASTEXITCODE
        }
    }

    if ($exitCode -eq 0) {
        & docker compose -f $composeFile exec -T go-runner go build ./...
        if ($LASTEXITCODE -ne 0) {
            $exitCode = $LASTEXITCODE
        }
    }
}
catch {
    Write-Host "[X] $($_.Exception.Message)"
    $exitCode = 1
}
finally {
    Set-Location $originalLocation
}

exit $exitCode
