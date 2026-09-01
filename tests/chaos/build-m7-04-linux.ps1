param(
    [Parameter(Mandatory = $true)]
    [string]$OutputDirectory,

    [switch]$AllowDirty
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$scriptDirectory = Split-Path -Parent $PSCommandPath
$repositoryRoot = (Resolve-Path (Join-Path $scriptDirectory '..\..')).Path
$originalLocation = Get-Location
$savedEnvironment = @{
    GOTOOLCHAIN = $env:GOTOOLCHAIN
    GOOS        = $env:GOOS
    GOARCH      = $env:GOARCH
    GOAMD64     = $env:GOAMD64
    CGO_ENABLED = $env:CGO_ENABLED
}

try {
    Set-Location $repositoryRoot
    $env:GOTOOLCHAIN = 'local'
    $versionCheckPath = Join-Path $repositoryRoot 'tools\check-go-version.ps1'
    $versionCheckText = [System.IO.File]::ReadAllText($versionCheckPath, [System.Text.Encoding]::UTF8)
    $versionCheck = [System.Management.Automation.ScriptBlock]::Create($versionCheckText)
    & $versionCheck
    if ($LASTEXITCODE -ne 0) {
        throw 'Go toolchain check failed.'
    }

    $worktreeStatus = @(git status --porcelain --untracked-files=all)
    if ($LASTEXITCODE -ne 0) {
        throw 'Unable to read Git worktree status.'
    }
    if (-not $AllowDirty -and $worktreeStatus.Count -gt 0) {
        throw 'A formal Linux filesystem failpoint build requires a clean worktree. Use -AllowDirty only for development smoke.'
    }

    if ([System.IO.Path]::IsPathRooted($OutputDirectory)) {
        $resolvedOutput = [System.IO.Path]::GetFullPath($OutputDirectory)
    }
    else {
        $resolvedOutput = [System.IO.Path]::GetFullPath((Join-Path $repositoryRoot $OutputDirectory))
    }
    New-Item -ItemType Directory -Force -Path $resolvedOutput | Out-Null

    $env:GOOS = 'linux'
    $env:GOARCH = 'amd64'
    $env:GOAMD64 = 'v1'
    $env:CGO_ENABLED = '0'

    $binaries = @(
        @{ Name = 'sqlite'; Package = './internal/repository/sqlite' }
        @{ Name = 'gateway'; Package = './internal/server/gateway' }
        @{ Name = 'durableops'; Package = './internal/server/durableops' }
    )
    $binaryHashes = @{}
    foreach ($binary in $binaries) {
        $name = $binary.Name
        $package = $binary.Package
        $binaryPath = Join-Path $resolvedOutput "$name.test"
        & go test -c -o $binaryPath $package
        if ($LASTEXITCODE -ne 0) {
            throw "Failed to build the Linux M7-04 $name test binary."
        }
        $binaryHashes[$name] = (Get-FileHash -Algorithm SHA256 $binaryPath).Hash.ToLowerInvariant()
    }

    $commit = (git rev-parse HEAD).Trim()
    $goVersion = (& go env GOVERSION).Trim()
    $toolchain = (& go env GOTOOLCHAIN).Trim()
    $clean = if ($worktreeStatus.Count -eq 0) { 'true' } else { 'false' }
    $manifestLines = @(
        "built_at_utc=$([DateTime]::UtcNow.ToString('yyyy-MM-ddTHH:mm:ssZ'))"
        "commit=$commit"
        "worktree_clean=$clean"
        "go_version=$goVersion"
        "toolchain=$toolchain"
        'goos=linux'
        'goarch=amd64'
        'goamd64=v1'
        'cgo_enabled=0'
        "sqlite_sha256=$($binaryHashes.sqlite)"
        "gateway_sha256=$($binaryHashes.gateway)"
        "durableops_sha256=$($binaryHashes.durableops)"
    )
    $manifestPath = Join-Path $resolvedOutput 'manifest.txt'
    $utf8NoBOM = [System.Text.UTF8Encoding]::new($false)
    $manifestText = [string]::Join("`n", [string[]]$manifestLines) + "`n"
    [System.IO.File]::WriteAllText($manifestPath, $manifestText, $utf8NoBOM)

    Write-Host "M7-04 Linux filesystem failpoint binaries: $resolvedOutput"
}
finally {
    foreach ($name in $savedEnvironment.Keys) {
        $value = $savedEnvironment[$name]
        if ($null -eq $value) {
            Remove-Item "Env:$name" -ErrorAction SilentlyContinue
        }
        else {
            Set-Item "Env:$name" $value
        }
    }
    Set-Location $originalLocation
}
