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
    & (Join-Path $repositoryRoot 'tools\check-go-version.ps1')
    if ($LASTEXITCODE -ne 0) {
        throw 'Go toolchain check failed.'
    }

    $beforeCommitOutput = @(git rev-parse HEAD)
    if ($LASTEXITCODE -ne 0 -or $beforeCommitOutput.Count -ne 1) {
        throw 'Unable to read the Git Commit before building.'
    }
    $beforeCommit = $beforeCommitOutput[0].Trim()
    $beforeTreeOutput = @(git rev-parse 'HEAD^{tree}')
    if ($LASTEXITCODE -ne 0 -or $beforeTreeOutput.Count -ne 1) {
        throw 'Unable to read the Git Tree before building.'
    }
    $beforeTree = $beforeTreeOutput[0].Trim()
    $worktreeStatus = @(git status --porcelain --untracked-files=all)
    if ($LASTEXITCODE -ne 0) {
        throw 'Unable to read Git worktree status.'
    }
    if (-not $AllowDirty -and $worktreeStatus.Count -gt 0) {
        throw 'A formal Linux leak build requires a clean worktree. Use -AllowDirty only for development smoke.'
    }

    $resolvedOutput = if ([IO.Path]::IsPathRooted($OutputDirectory)) {
        [IO.Path]::GetFullPath($OutputDirectory)
    }
    else {
        [IO.Path]::GetFullPath((Join-Path $repositoryRoot $OutputDirectory))
    }
    $repositoryPrefix = $repositoryRoot.TrimEnd('\') + '\'
    if ($resolvedOutput.Equals($repositoryRoot, [StringComparison]::OrdinalIgnoreCase) -or
        $resolvedOutput.StartsWith($repositoryPrefix, [StringComparison]::OrdinalIgnoreCase)) {
        throw 'OutputDirectory must be outside the repository.'
    }
    if (Test-Path -LiteralPath $resolvedOutput) {
        if (@(Get-ChildItem -LiteralPath $resolvedOutput -Force).Count -ne 0) {
            throw 'OutputDirectory must be new or empty.'
        }
    }
    else {
        New-Item -ItemType Directory -Path $resolvedOutput | Out-Null
    }

    $env:GOOS = 'linux'
    $env:GOARCH = 'amd64'
    $env:GOAMD64 = 'v1'
    $env:CGO_ENABLED = '0'
    $binaryPath = Join-Path $resolvedOutput 'bootstrap.test'
    & go test -c -o $binaryPath ./internal/server/bootstrap
    if ($LASTEXITCODE -ne 0) {
        throw 'Failed to build the Linux M7-07 leak test binary.'
    }

    $afterCommitOutput = @(git rev-parse HEAD)
    if ($LASTEXITCODE -ne 0 -or $afterCommitOutput.Count -ne 1) {
        throw 'Unable to read the Git Commit after building.'
    }
    $afterCommit = $afterCommitOutput[0].Trim()
    $afterTreeOutput = @(git rev-parse 'HEAD^{tree}')
    if ($LASTEXITCODE -ne 0 -or $afterTreeOutput.Count -ne 1) {
        throw 'Unable to read the Git Tree after building.'
    }
    $afterTree = $afterTreeOutput[0].Trim()
    $afterStatus = @(git status --porcelain --untracked-files=all)
    if ($LASTEXITCODE -ne 0) {
        throw 'Unable to read Git worktree status after building.'
    }
    if ($afterCommit -ne $beforeCommit -or $afterTree -ne $beforeTree -or
        [string]::Join("`n", [string[]]$afterStatus) -ne [string]::Join("`n", [string[]]$worktreeStatus)) {
        throw 'Repository Commit, Tree, or worktree changed while building the M7-07 binary.'
    }

    $manifestLines = @(
        "built_at_utc=$([DateTime]::UtcNow.ToString('yyyy-MM-ddTHH:mm:ssZ'))"
        "commit=$beforeCommit"
        "tree=$beforeTree"
        "worktree_clean=$(if ($worktreeStatus.Count -eq 0) { 'true' } else { 'false' })"
        "go_version=$((& go env GOVERSION).Trim())"
        "toolchain=$((& go env GOTOOLCHAIN).Trim())"
        'goos=linux'
        'goarch=amd64'
        'goamd64=v1'
        'cgo_enabled=0'
        "bootstrap_sha256=$((Get-FileHash -Algorithm SHA256 $binaryPath).Hash.ToLowerInvariant())"
    )
    $utf8NoBOM = [Text.UTF8Encoding]::new($false)
    [IO.File]::WriteAllText(
        (Join-Path $resolvedOutput 'manifest.txt'),
        [string]::Join("`n", [string[]]$manifestLines) + "`n",
        $utf8NoBOM
    )
    Write-Host "M7-07 Linux leak binary: $resolvedOutput"
}
finally {
    foreach ($name in $savedEnvironment.Keys) {
        if ($null -eq $savedEnvironment[$name]) {
            Remove-Item "Env:$name" -ErrorAction SilentlyContinue
        }
        else {
            Set-Item "Env:$name" $savedEnvironment[$name]
        }
    }
    Set-Location $originalLocation
}
