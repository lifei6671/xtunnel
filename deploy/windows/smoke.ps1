[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$AgentPath
)

# Repository smoke harness only. End users install the service with the Agent executable.
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$serviceName = 'XTunnelAgent'
$installDirectory = Join-Path $env:ProgramFiles 'XTunnel'
$installedBinary = Join-Path $installDirectory 'xtunnel-agent.exe'
$productDataDirectory = Join-Path $env:ProgramData 'XTunnel'
$credentialDirectory = Join-Path $productDataDirectory 'credentials'
$credentialPath = Join-Path $credentialDirectory 'agent.token.dpapi'
$agentFullPath = [IO.Path]::GetFullPath($AgentPath)
$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..\..'))
$installAttempted = $false
$uninstallCompleted = $false
$primaryFailure = $null
$cleanupFailures = New-Object System.Collections.Generic.List[string]

function Get-AgentService {
    Get-CimInstance -ClassName Win32_Service -Filter "Name='$serviceName'" -ErrorAction SilentlyContinue
}

function Wait-AgentServiceStatus {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Status,

        [int]$TimeoutSeconds = 30
    )

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    while ([DateTime]::UtcNow -lt $deadline) {
        $service = Get-AgentService
        if (($null -ne $service) -and ($service.State -eq $Status)) {
            return
        }
        Start-Sleep -Milliseconds 250
    }
    throw "service $serviceName did not reach state $Status"
}

function Wait-AgentServiceDeleted {
    param(
        [int]$TimeoutSeconds = 30
    )

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    while ([DateTime]::UtcNow -lt $deadline) {
        if ($null -eq (Get-AgentService)) {
            return
        }
        Start-Sleep -Milliseconds 250
    }
    throw "service $serviceName was not deleted"
}

function Assert-AgentServiceStable {
    $before = Get-AgentService
    if (($null -eq $before) -or ($before.State -ne 'Running') -or ($before.ProcessId -eq 0)) {
        throw "service $serviceName is not running with a process"
    }
    $processID = $before.ProcessId
    Start-Sleep -Seconds 1
    $after = Get-AgentService
    if (($null -eq $after) -or ($after.State -ne 'Running') -or ($after.ProcessId -ne $processID)) {
        throw "service $serviceName did not remain stable"
    }
}

function Invoke-Agent {
    param(
        [Parameter(Mandatory = $true)]
        [string[]]$ArgumentList
    )

    $output = & $agentFullPath @ArgumentList 2>&1
    if ($LASTEXITCODE -ne 0) {
        $message = ($output | ForEach-Object { $_.ToString() }) -join [Environment]::NewLine
        throw "Agent command failed with exit code $LASTEXITCODE`: $message"
    }
}

function New-SmokeToken {
    # The Connection Token wire format belongs to the Go protocol package. Reuse its
    # production encoder instead of duplicating Protobuf and HMAC rules in PowerShell.
    $token = & go -C $repositoryRoot run ./deploy/windows/smoketoken
    if ($LASTEXITCODE -ne 0) {
        throw "failed to generate Windows smoke Connection Token (exit code $LASTEXITCODE)"
    }
    if (($null -eq $token) -or ([string]::IsNullOrWhiteSpace($token.ToString()))) {
        throw 'Windows smoke Connection Token generator returned no Token'
    }
    return $token.ToString().Trim()
}

function Assert-ServiceContract {
    $service = Get-AgentService
    if ($null -eq $service) {
        throw "service $serviceName is missing"
    }
    if ($service.StartMode -ne 'Auto') {
        throw "service $serviceName is not configured for automatic start"
    }
    if ($service.StartName -ne 'NT AUTHORITY\LocalService') {
        throw "service $serviceName does not use LocalService"
    }

    $escapedBinary = [Regex]::Escape($installedBinary)
    if ($service.PathName -notmatch ('^"' + $escapedBinary + '"\s+run$')) {
        throw "service $serviceName has an unexpected executable command"
    }
    if (($service.PathName -match '--token') -or ($service.PathName -match 'xta_')) {
        throw "service $serviceName command must not contain a Token"
    }
}

function Assert-CredentialProtected {
    param(
        [Parameter(Mandatory = $true)]
        [string]$PlaintextToken
    )

    $item = Get-Item -LiteralPath $credentialPath -Force
    if (-not $item.PSIsContainer -and ($item.Length -gt 0)) {
        $protectedBytes = [IO.File]::ReadAllBytes($credentialPath)
        $plaintextBytes = [Text.Encoding]::UTF8.GetBytes($PlaintextToken)
        if ([Convert]::ToBase64String($protectedBytes) -eq [Convert]::ToBase64String($plaintextBytes)) {
            throw 'stored Agent credential is plaintext'
        }
        $decodedProtectedBytes = [Text.Encoding]::UTF8.GetString($protectedBytes)
        if ($decodedProtectedBytes.Contains($PlaintextToken)) {
            throw 'stored Agent credential contains the plaintext Token'
        }
        return
    }
    throw 'stored Agent credential is missing or invalid'
}

function Assert-RestrictedAcl {
    param(
        [Parameter(Mandatory = $true)]
        [string]$LiteralPath
    )

    $acl = Get-Acl -LiteralPath $LiteralPath
    if (-not $acl.AreAccessRulesProtected) {
        throw "ACL inheritance is enabled for $LiteralPath"
    }
    $allowedSids = @('S-1-5-18', 'S-1-5-32-544', 'S-1-5-19')
    $seenSids = @{}
    foreach ($rule in @($acl.Access)) {
        if ($rule.AccessControlType -ne [Security.AccessControl.AccessControlType]::Allow) {
            continue
        }
        $sid = $rule.IdentityReference.Translate([Security.Principal.SecurityIdentifier]).Value
        if ($allowedSids -notcontains $sid) {
            throw "unexpected Allow ACL principal on $LiteralPath"
        }
        $seenSids[$sid] = $true
    }
    foreach ($requiredSid in $allowedSids) {
        if (-not $seenSids.ContainsKey($requiredSid)) {
            throw "required ACL principal $requiredSid is missing from $LiteralPath"
        }
    }
}

function Remove-EmptyDirectory {
    param(
        [Parameter(Mandatory = $true)]
        [string]$LiteralPath
    )

    if (-not (Test-Path -LiteralPath $LiteralPath -PathType Container)) {
        return
    }
    $entries = @(Get-ChildItem -LiteralPath $LiteralPath -Force)
    if ($entries.Count -ne 0) {
        throw "refusing to remove non-empty directory $LiteralPath"
    }
    [IO.Directory]::Delete($LiteralPath, $false)
}

if (-not [Environment]::Is64BitOperatingSystem) {
    throw 'Windows Service smoke requires 64-bit Windows'
}
if (-not (Test-Path -LiteralPath $agentFullPath -PathType Leaf)) {
    throw "Agent executable not found: $agentFullPath"
}

$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
$principal = New-Object Security.Principal.WindowsPrincipal($identity)
$administrator = [Security.Principal.WindowsBuiltInRole]::Administrator
if (-not $principal.IsInRole($administrator)) {
    throw 'Windows Service smoke must run from an elevated Administrator session'
}
if ($null -ne (Get-AgentService)) {
    throw "refusing to overwrite existing service $serviceName"
}
if ((Test-Path -LiteralPath $installDirectory) -or (Test-Path -LiteralPath $productDataDirectory)) {
    throw 'refusing to overwrite an existing XTunnel install or data path'
}

try {
    $firstToken = New-SmokeToken
    $installAttempted = $true
    Invoke-Agent -ArgumentList @('service', 'install', '--token', $firstToken)
    Wait-AgentServiceStatus -Status 'Running'
    Assert-AgentServiceStable
    Assert-ServiceContract
    Assert-CredentialProtected -PlaintextToken $firstToken
    Assert-RestrictedAcl -LiteralPath $credentialDirectory
    Assert-RestrictedAcl -LiteralPath $credentialPath
    if (-not (Test-Path -LiteralPath $installedBinary -PathType Leaf)) {
        throw 'installed Agent binary is missing'
    }
    $sourceBinaryHash = (Get-FileHash -LiteralPath $agentFullPath -Algorithm SHA256).Hash
    $installedBinaryHash = (Get-FileHash -LiteralPath $installedBinary -Algorithm SHA256).Hash
    if ($sourceBinaryHash -ne $installedBinaryHash) {
        throw 'installed Agent binary does not match the source executable'
    }

    $firstCredentialHash = (Get-FileHash -LiteralPath $credentialPath -Algorithm SHA256).Hash
    $secondToken = New-SmokeToken
    Invoke-Agent -ArgumentList @('service', 'install', '--token', $secondToken)
    Wait-AgentServiceStatus -Status 'Running'
    Assert-AgentServiceStable
    Assert-ServiceContract
    Assert-CredentialProtected -PlaintextToken $secondToken
    Assert-RestrictedAcl -LiteralPath $credentialDirectory
    Assert-RestrictedAcl -LiteralPath $credentialPath
    $secondCredentialHash = (Get-FileHash -LiteralPath $credentialPath -Algorithm SHA256).Hash
    if ($firstCredentialHash -eq $secondCredentialHash) {
        throw 'reinstall did not replace the protected Agent credential'
    }

    Stop-Service -Name $serviceName
    Wait-AgentServiceStatus -Status 'Stopped'
    Start-Service -Name $serviceName
    Wait-AgentServiceStatus -Status 'Running'
    Assert-AgentServiceStable

    Invoke-Agent -ArgumentList @('service', 'uninstall')
    $uninstallCompleted = $true
    Wait-AgentServiceDeleted
    if (Test-Path -LiteralPath $installedBinary) {
        throw 'installed Agent binary still exists after uninstall'
    }
    if (-not (Test-Path -LiteralPath $credentialPath -PathType Leaf)) {
        throw 'uninstall must preserve the protected Agent credential'
    }
}
catch {
    $primaryFailure = $_
}
finally {
    if ($installAttempted -and (-not $uninstallCompleted)) {
        try {
            Invoke-Agent -ArgumentList @('service', 'uninstall')
            $uninstallCompleted = $true
        }
        catch {
            $cleanupFailures.Add("managed uninstall failed: $($_.Exception.Message)")
        }
    }

    $remainingService = Get-AgentService
    if ($null -ne $remainingService) {
        $escapedBinary = [Regex]::Escape($installedBinary)
        if ($remainingService.PathName -match ('^"' + $escapedBinary + '"\s+run$')) {
            try {
                if ($remainingService.State -ne 'Stopped') {
                    & sc.exe stop $serviceName | Out-Null
                    Wait-AgentServiceStatus -Status 'Stopped'
                }
                & sc.exe delete $serviceName | Out-Null
                if ($LASTEXITCODE -ne 0) {
                    throw "sc.exe delete failed with exit code $LASTEXITCODE"
                }
                Wait-AgentServiceDeleted
            }
            catch {
                $cleanupFailures.Add("service cleanup failed: $($_.Exception.Message)")
            }
        }
        else {
            $cleanupFailures.Add('refusing to remove service with an unexpected executable command')
        }
    }

    foreach ($path in @($installedBinary, $credentialPath)) {
        if (Test-Path -LiteralPath $path -PathType Leaf) {
            try {
                Remove-Item -LiteralPath $path -Force
            }
            catch {
                $cleanupFailures.Add("file cleanup failed for $path`: $($_.Exception.Message)")
            }
        }
    }
    foreach ($directory in @($credentialDirectory, $productDataDirectory, $installDirectory)) {
        try {
            Remove-EmptyDirectory -LiteralPath $directory
        }
        catch {
            $cleanupFailures.Add($_.Exception.Message)
        }
    }
}

if ($null -ne $primaryFailure) {
    if ($cleanupFailures.Count -ne 0) {
        $cleanupMessage = $cleanupFailures -join [Environment]::NewLine
        throw "Windows Agent service smoke failed: $($primaryFailure.Exception.Message)$([Environment]::NewLine)Cleanup also failed:$([Environment]::NewLine)$cleanupMessage"
    }
    throw $primaryFailure
}
if ($cleanupFailures.Count -ne 0) {
    throw ($cleanupFailures -join [Environment]::NewLine)
}

Write-Output '[OK] Windows Agent service smoke passed'
