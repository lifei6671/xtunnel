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
        & docker compose -f $composeFile exec -T go-runner sh ./tools/check-go-version.sh
        if ($LASTEXITCODE -ne 0) {
            $exitCode = $LASTEXITCODE
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
