[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$AgentPath,

    [Parameter(Mandatory = $true)]
    [string]$GateHelperPath
)

# Repository smoke harness only. End users install the service with the Agent executable.
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$serviceName = 'XTunnelAgent'
$eventSourceName = 'XTunnelAgent'
$eventSourceRegistryPath = 'Registry::HKEY_LOCAL_MACHINE\SYSTEM\CurrentControlSet\Services\EventLog\Application\XTunnelAgent'
$serviceRegistryPath = 'Registry::HKEY_LOCAL_MACHINE\SYSTEM\CurrentControlSet\Services\XTunnelAgent'
$sessionManagerRegistryPath = 'Registry::HKEY_LOCAL_MACHINE\SYSTEM\CurrentControlSet\Control\Session Manager'
$pendingRenameValueName = 'PendingFileRenameOperations'
$managedMarker = 'Managed by xtunnel-agent service install'
$installDirectory = Join-Path $env:ProgramFiles 'XTunnel'
$installedBinary = Join-Path $installDirectory 'xtunnel-agent.exe'
$productDataDirectory = Join-Path $env:ProgramData 'XTunnel'
$credentialDirectory = Join-Path $productDataDirectory 'credentials'
$credentialPath = Join-Path $credentialDirectory 'agent.token.dpapi'
$agentFullPath = [IO.Path]::GetFullPath($AgentPath)
$gateHelperFullPath = [IO.Path]::GetFullPath($GateHelperPath)
$installedGateHelper = Join-Path $installDirectory 'xtunnel-scm-gate-helper.exe'
$gateDirectory = Join-Path $productDataDirectory 'scm-gate'
$expectedAgentCommand = '"' + $installedBinary + '" run'
$repositoryRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..\..'))
$installAttempted = $false
$uninstallCompleted = $false
$preserveInstalledBinary = $false
$primaryFailure = $null
$cleanupFailures = New-Object System.Collections.Generic.List[string]
$eventQueryStart = [DateTime]::UtcNow.AddSeconds(-2)

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

function Wait-AgentServiceFailure {
    param(
        [int]$TimeoutSeconds = 4
    )

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    while ([DateTime]::UtcNow -lt $deadline) {
        $service = Get-AgentService
        if (($null -ne $service) -and ($service.State -eq 'Stopped')) {
            if ($service.ExitCode -eq 0) {
                throw "service $serviceName startup failure reported ExitCode=0"
            }
            return $service
        }
        Start-Sleep -Milliseconds 100
    }
    throw "service $serviceName did not expose a stopped failure before recovery"
}

function Wait-AgentServiceProcessChange {
    param(
        [Parameter(Mandatory = $true)]
        [uint32]$PreviousProcessId,

        [int]$TimeoutSeconds = 15
    )

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    while ([DateTime]::UtcNow -lt $deadline) {
        $service = Get-AgentService
        if (($null -ne $service) -and ($service.State -eq 'Running') -and
            ($service.ProcessId -ne 0) -and ($service.ProcessId -ne $PreviousProcessId)) {
            return $service
        }
        Start-Sleep -Milliseconds 100
    }
    throw "SCM recovery did not start a new $serviceName process after PID $PreviousProcessId"
}

function Set-AgentServiceCommand {
    param(
        [Parameter(Mandatory = $true)]
        [string]$CommandLine
    )

    $service = Get-AgentService
    if ($null -eq $service) {
        throw "service $serviceName is missing"
    }
    # Windows PowerShell 5.1 cannot reliably preserve the embedded quotes in a
    # service ImagePath when forwarding a string argument through sc.exe.
    $change = Invoke-CimMethod -InputObject $service -MethodName Change `
        -Arguments @{ PathName = $CommandLine }
    if ($change.ReturnValue -ne 0) {
        throw "Win32_Service.Change failed with return value $($change.ReturnValue)"
    }

    $deadline = [DateTime]::UtcNow.AddSeconds(5)
    while ([DateTime]::UtcNow -lt $deadline) {
        $service = Get-AgentService
        if (($null -ne $service) -and
            [string]::Equals($service.PathName, $CommandLine, [StringComparison]::OrdinalIgnoreCase)) {
            return
        }
        Start-Sleep -Milliseconds 100
    }
    throw "service $serviceName did not publish the expected executable command"
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

function Invoke-AgentExpectFailure {
    param(
        [Parameter(Mandatory = $true)]
        [string[]]$ArgumentList,

        [Parameter(Mandatory = $true)]
        [string]$ExpectedMessage,

        [string[]]$ForbiddenValues = @()
    )

    # Windows PowerShell 5.1 promotes native stderr to a terminating error when
    # ErrorActionPreference is Stop. Expected product failures must still be captured.
    $previousErrorActionPreference = $ErrorActionPreference
    try {
        $ErrorActionPreference = 'Continue'
        $output = & $agentFullPath @ArgumentList 2>&1
        $exitCode = $LASTEXITCODE
    }
    finally {
        $ErrorActionPreference = $previousErrorActionPreference
    }
    $message = ($output | ForEach-Object { $_.ToString() }) -join [Environment]::NewLine
    foreach ($forbidden in $ForbiddenValues) {
        if ((-not [string]::IsNullOrEmpty($forbidden)) -and $message.Contains($forbidden)) {
            throw 'expected Agent command failure exposed a forbidden value'
        }
    }
    if ($exitCode -eq 0) {
        throw 'Agent command unexpectedly accepted an unmanaged Windows object'
    }
    if (-not $message.Contains($ExpectedMessage)) {
        throw 'Agent command did not report the expected unmanaged-object refusal'
    }
}

function Get-AgentServiceSnapshot {
    $service = Get-AgentService
    if ($null -eq $service) {
        return $null
    }
    $serviceKey = Get-Item -LiteralPath $serviceRegistryPath
    $valueSignature = @($serviceKey.GetValueNames() | Sort-Object | ForEach-Object {
            "$_`:$($serviceKey.GetValueKind($_))"
        }) -join ';'
    return [PSCustomObject]@{
        DisplayName = $service.DisplayName
        PathName = $service.PathName
        StartMode = $service.StartMode
        StartName = $service.StartName
        State = $service.State
        Description = $service.Description
        ServiceType = $service.ServiceType
        ErrorControl = $service.ErrorControl
        ValueSignature = $valueSignature
    }
}

function Test-AgentServiceSnapshotEqual {
    param(
        [Parameter(Mandatory = $true)]
        [PSCustomObject]$Left,

        [Parameter(Mandatory = $true)]
        [PSCustomObject]$Right
    )

    foreach ($property in @(
            'DisplayName',
            'PathName',
            'StartMode',
            'StartName',
            'State',
            'Description',
            'ServiceType',
            'ErrorControl',
            'ValueSignature'
        )) {
        if (-not [object]::Equals($Left.$property, $Right.$property)) {
            return $false
        }
    }
    return $true
}

function Test-UnmanagedServiceBoundary {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Token
    )

    $foreignDisplayName = "XTunnel unmanaged smoke service $PID"
    $foreignDescription = "XTunnel unmanaged smoke owner $PID"
    $foreignBinary = Join-Path $env:SystemRoot 'System32\cmd.exe'
    $foreignCommand = '"' + $foreignBinary + '" /c exit 0'
    $ownedSnapshot = $null
    $testFailure = $null
    $cleanupFailure = $null

    try {
        New-Service -Name $serviceName -BinaryPathName $foreignCommand `
            -DisplayName $foreignDisplayName -StartupType Manual | Out-Null
        $ownedSnapshot = Get-AgentServiceSnapshot
        if ($null -eq $ownedSnapshot) {
            throw 'unmanaged smoke service was not created'
        }
        $descriptionOutput = & sc.exe description $serviceName $foreignDescription 2>&1
        if ($LASTEXITCODE -ne 0) {
            throw 'failed to mark the unmanaged smoke service'
        }
        $markedSnapshot = Get-AgentServiceSnapshot
        if ($null -eq $markedSnapshot) {
            throw 'unmanaged smoke service disappeared after ownership marking'
        }
        $ownedSnapshot = $markedSnapshot

        Invoke-AgentExpectFailure -ArgumentList @('service', 'install', '--token', $Token) `
            -ExpectedMessage 'refusing to overwrite an unmanaged or modified XTunnelAgent service' `
            -ForbiddenValues @($Token)
        $afterInstall = Get-AgentServiceSnapshot
        if (($null -eq $afterInstall) -or
            (-not (Test-AgentServiceSnapshotEqual -Left $ownedSnapshot -Right $afterInstall))) {
            throw 'failed install modified the unmanaged smoke service'
        }
        if ((Test-Path -LiteralPath $eventSourceRegistryPath) -or
            (Test-Path -LiteralPath $installDirectory) -or
            (Test-Path -LiteralPath $productDataDirectory)) {
            throw 'failed install created managed artifacts beside the unmanaged smoke service'
        }

        Invoke-AgentExpectFailure -ArgumentList @('service', 'uninstall') `
            -ExpectedMessage 'refusing to remove an unmanaged or modified XTunnelAgent service'
        $afterUninstall = Get-AgentServiceSnapshot
        if (($null -eq $afterUninstall) -or
            (-not (Test-AgentServiceSnapshotEqual -Left $ownedSnapshot -Right $afterUninstall))) {
            throw 'failed uninstall modified the unmanaged smoke service'
        }
        if ((Test-Path -LiteralPath $eventSourceRegistryPath) -or
            (Test-Path -LiteralPath $installDirectory) -or
            (Test-Path -LiteralPath $productDataDirectory)) {
            throw 'failed uninstall created managed artifacts beside the unmanaged smoke service'
        }
    }
    catch {
        $testFailure = $_
    }
    finally {
        $remaining = Get-AgentServiceSnapshot
        if ($null -ne $remaining) {
            if (($null -ne $ownedSnapshot) -and
                (Test-AgentServiceSnapshotEqual -Left $ownedSnapshot -Right $remaining)) {
                $deleteOutput = & sc.exe delete $serviceName 2>&1
                if ($LASTEXITCODE -eq 0) {
                    try {
                        Wait-AgentServiceDeleted
                    }
                    catch {
                        $cleanupFailure = $_
                    }
                }
                else {
                    $cleanupFailure = [InvalidOperationException]::new('failed to delete the owned unmanaged smoke service')
                }
            }
            else {
                $cleanupFailure = [InvalidOperationException]::new('refusing to delete an unmanaged smoke service whose ownership changed')
            }
        }
    }

    if ($null -ne $testFailure) {
        if ($null -ne $cleanupFailure) {
            throw "unmanaged Service boundary failed: $($testFailure.Exception.Message); cleanup also failed: $($cleanupFailure.Exception.Message)"
        }
        throw $testFailure
    }
    if ($null -ne $cleanupFailure) {
        throw $cleanupFailure
    }
}

function Get-UnmanagedEventSourceSnapshot {
    if (-not (Test-Path -LiteralPath $eventSourceRegistryPath)) {
        return $null
    }
    $sourceKey = Get-Item -LiteralPath $eventSourceRegistryPath
    $source = Get-ItemProperty -LiteralPath $eventSourceRegistryPath
    $valueSignature = @($sourceKey.GetValueNames() | Sort-Object | ForEach-Object {
            "$_`:$($sourceKey.GetValueKind($_))"
        }) -join ';'
    return [PSCustomObject]@{
        XTunnelManaged = $source.XTunnelManaged
        CustomSource = [int]$source.CustomSource
        EventMessageFile = $source.EventMessageFile
        TypesSupported = [int]$source.TypesSupported
        ForeignSmokeOwner = $source.ForeignSmokeOwner
        ValueSignature = $valueSignature
    }
}

function Test-EventSourceSnapshotEqual {
    param(
        [Parameter(Mandatory = $true)]
        [PSCustomObject]$Left,

        [Parameter(Mandatory = $true)]
        [PSCustomObject]$Right
    )

    foreach ($property in @(
            'XTunnelManaged',
            'CustomSource',
            'EventMessageFile',
            'TypesSupported',
            'ForeignSmokeOwner',
            'ValueSignature'
        )) {
        if (-not [object]::Equals($Left.$property, $Right.$property)) {
            return $false
        }
    }
    return $true
}

function Test-UnmanagedEventSourceBoundary {
    param(
        [Parameter(Mandatory = $true)]
        [string]$Token
    )

    $foreignOwner = "XTunnel unmanaged Event Source owner $PID"
    $ownedSnapshot = $null
    $testFailure = $null
    $cleanupFailure = $null

    try {
        New-Item -Path $eventSourceRegistryPath | Out-Null
        New-ItemProperty -LiteralPath $eventSourceRegistryPath -Name 'ForeignSmokeOwner' `
            -Value $foreignOwner -PropertyType String | Out-Null
        New-ItemProperty -LiteralPath $eventSourceRegistryPath -Name 'XTunnelManaged' `
            -Value $foreignOwner -PropertyType String | Out-Null
        New-ItemProperty -LiteralPath $eventSourceRegistryPath -Name 'CustomSource' `
            -Value 0 -PropertyType DWord | Out-Null
        New-ItemProperty -LiteralPath $eventSourceRegistryPath -Name 'EventMessageFile' `
            -Value 'foreign-message-file' -PropertyType ExpandString | Out-Null
        New-ItemProperty -LiteralPath $eventSourceRegistryPath -Name 'TypesSupported' `
            -Value 1 -PropertyType DWord | Out-Null
        $ownedSnapshot = Get-UnmanagedEventSourceSnapshot

        Invoke-AgentExpectFailure -ArgumentList @('service', 'install', '--token', $Token) `
            -ExpectedMessage 'refusing to overwrite an unmanaged or modified XTunnelAgent Event Log Source' `
            -ForbiddenValues @($Token)
        $afterInstall = Get-UnmanagedEventSourceSnapshot
        if (($null -eq $afterInstall) -or
            (-not (Test-EventSourceSnapshotEqual -Left $ownedSnapshot -Right $afterInstall))) {
            throw 'failed install modified the unmanaged Event Log Source'
        }
        if (($null -ne (Get-AgentService)) -or
            (Test-Path -LiteralPath $installDirectory) -or
            (Test-Path -LiteralPath $productDataDirectory)) {
            throw 'failed install created managed artifacts beside the unmanaged Event Log Source'
        }

        Invoke-AgentExpectFailure -ArgumentList @('service', 'uninstall') `
            -ExpectedMessage 'refusing to remove an unmanaged or modified XTunnelAgent Event Log Source'
        $afterUninstall = Get-UnmanagedEventSourceSnapshot
        if (($null -eq $afterUninstall) -or
            (-not (Test-EventSourceSnapshotEqual -Left $ownedSnapshot -Right $afterUninstall))) {
            throw 'failed uninstall modified the unmanaged Event Log Source'
        }
        if (($null -ne (Get-AgentService)) -or
            (Test-Path -LiteralPath $installDirectory) -or
            (Test-Path -LiteralPath $productDataDirectory)) {
            throw 'failed uninstall created managed artifacts beside the unmanaged Event Log Source'
        }
    }
    catch {
        $testFailure = $_
    }
    finally {
        if (Test-Path -LiteralPath $eventSourceRegistryPath) {
            $remainingSnapshot = Get-UnmanagedEventSourceSnapshot
            if (($null -ne $ownedSnapshot) -and
                (Test-EventSourceSnapshotEqual -Left $ownedSnapshot -Right $remainingSnapshot)) {
                try {
                    Remove-Item -LiteralPath $eventSourceRegistryPath -Force
                }
                catch {
                    $cleanupFailure = $_
                }
            }
            else {
                $cleanupFailure = [InvalidOperationException]::new('refusing to remove an unmanaged Event Source whose ownership changed')
            }
        }
    }

    if ($null -ne $testFailure) {
        if ($null -ne $cleanupFailure) {
            throw "unmanaged Event Source boundary failed: $($testFailure.Exception.Message); cleanup also failed: $($cleanupFailure.Exception.Message)"
        }
        throw $testFailure
    }
    if ($null -ne $cleanupFailure) {
        throw $cleanupFailure
    }
}

function Get-PendingRenameSnapshot {
    $key = Get-Item -LiteralPath $sessionManagerRegistryPath
    $valueNames = @($key.GetValueNames())
    if ($valueNames -notcontains $pendingRenameValueName) {
        return [PSCustomObject]@{
            Exists = $false
            Values = [string[]]@()
        }
    }

    $rawValue = $key.GetValue(
        $pendingRenameValueName,
        $null,
        [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames
    )
    if ($rawValue -isnot [string[]]) {
        throw "$pendingRenameValueName is not a REG_MULTI_SZ value"
    }
    return [PSCustomObject]@{
        Exists = $true
        Values = [string[]]$rawValue
    }
}

function Test-PendingRenameSnapshotEqual {
    param(
        [Parameter(Mandatory = $true)]
        [PSCustomObject]$Left,

        [Parameter(Mandatory = $true)]
        [PSCustomObject]$Right
    )

    if ($Left.Exists -ne $Right.Exists) {
        return $false
    }
    if ($Left.Values.Count -ne $Right.Values.Count) {
        return $false
    }
    for ($index = 0; $index -lt $Left.Values.Count; $index++) {
        if (-not [string]::Equals(
                $Left.Values[$index],
                $Right.Values[$index],
                [StringComparison]::Ordinal
            )) {
            return $false
        }
    }
    return $true
}

function Test-PendingDeleteDelta {
    param(
        [Parameter(Mandatory = $true)]
        [PSCustomObject]$Before,

        [Parameter(Mandatory = $true)]
        [PSCustomObject]$After,

        [Parameter(Mandatory = $true)]
        [string]$BinaryPath
    )

    if (-not $After.Exists) {
        return $false
    }
    if (($After.Values.Count -lt ($Before.Values.Count + 1)) -or
        ($After.Values.Count -gt ($Before.Values.Count + 2))) {
        return $false
    }
    for ($index = 0; $index -lt $Before.Values.Count; $index++) {
        if (-not [string]::Equals(
                $Before.Values[$index],
                $After.Values[$index],
                [StringComparison]::Ordinal
            )) {
            return $false
        }
    }

    $scheduledSource = '\??\' + $BinaryPath
    $sourceCount = 0
    for ($index = $Before.Values.Count; $index -lt $After.Values.Count; $index++) {
        if ([string]::Equals(
                $After.Values[$index],
                $scheduledSource,
                [StringComparison]::OrdinalIgnoreCase
            )) {
            $sourceCount++
        }
        elseif (-not [string]::IsNullOrEmpty($After.Values[$index])) {
            return $false
        }
    }
    # REG_MULTI_SZ readers omit the final terminator. If the previous operation
    # was also a delete, its formerly trailing empty destination becomes visible
    # before this newly appended source. Accept only that empty value plus the
    # exact task-owned source; every pre-existing entry must still match above.
    return $sourceCount -eq 1
}

function Restore-PendingRenameSnapshot {
    param(
        [Parameter(Mandatory = $true)]
        [PSCustomObject]$Before,

        [Parameter(Mandatory = $true)]
        [PSCustomObject]$OwnedAfter
    )

    $current = Get-PendingRenameSnapshot
    if (-not (Test-PendingRenameSnapshotEqual -Left $OwnedAfter -Right $current)) {
        throw 'refusing to restore PendingFileRenameOperations after concurrent modification'
    }
    if ($Before.Exists) {
        $key = Get-Item -LiteralPath $sessionManagerRegistryPath
        $key.SetValue(
            $pendingRenameValueName,
            [string[]]$Before.Values,
            [Microsoft.Win32.RegistryValueKind]::MultiString
        )
    }
    else {
        Remove-ItemProperty -LiteralPath $sessionManagerRegistryPath `
            -Name $pendingRenameValueName
    }

    $restored = Get-PendingRenameSnapshot
    if (-not (Test-PendingRenameSnapshotEqual -Left $Before -Right $restored)) {
        throw 'PendingFileRenameOperations was not restored to its pre-smoke value'
    }
}

function Invoke-InstalledAgentSelfUninstall {
    # All ownership probes are fallible. Fail closed before touching the installed path.
    $script:preserveInstalledBinary = $true
    $ownedBinary = Get-Item -LiteralPath $installedBinary
    $ownedBinaryHash = (Get-FileHash -LiteralPath $installedBinary -Algorithm SHA256).Hash
    $beforePendingRename = Get-PendingRenameSnapshot
    $afterPendingRename = $null
    $ownsPendingDelete = $false
    $removedOwnedBinary = $false
    $testFailure = $null
    $cleanupFailures = New-Object System.Collections.Generic.List[string]

    try {
        $output = & $installedBinary service uninstall 2>&1
        $exitCode = $LASTEXITCODE
        $message = ($output | ForEach-Object { $_.ToString() }) -join [Environment]::NewLine
        $afterPendingRename = Get-PendingRenameSnapshot
        $ownsPendingDelete = Test-PendingDeleteDelta `
            -Before $beforePendingRename -After $afterPendingRename -BinaryPath $installedBinary

        if ($exitCode -ne 0) {
            throw "installed Agent self-uninstall failed with exit code $exitCode`: $message"
        }
        if ((-not $message.Contains('the running binary will be deleted after the next reboot')) -or
            (-not $message.Contains('the credential was preserved'))) {
            throw 'installed Agent self-uninstall did not report delayed binary removal'
        }
        Wait-AgentServiceDeleted
        if (Test-Path -LiteralPath $eventSourceRegistryPath) {
            throw 'self-uninstall left the managed Windows Event Log Source behind'
        }
        if (-not (Test-Path -LiteralPath $credentialPath -PathType Leaf)) {
            throw 'self-uninstall did not preserve the protected Agent credential'
        }
        # The product entry removed the SCM Service and managed Event Source. Later
        # evidence or cleanup failures must not retry uninstall from the source binary.
        $script:uninstallCompleted = $true
        if (-not (Test-Path -LiteralPath $installedBinary -PathType Leaf)) {
            throw 'self-uninstall reported delayed removal but the running binary disappeared immediately'
        }
        if (-not $ownsPendingDelete) {
            $entryShape = @($afterPendingRename.Values | Select-Object -First 16 | ForEach-Object {
                    if ([string]::IsNullOrEmpty($_)) {
                        'empty'
                    }
                    elseif ($_.EndsWith(
                            [IO.Path]::GetFileName($installedBinary),
                            [StringComparison]::OrdinalIgnoreCase
                        )) {
                        'task-leaf'
                    }
                    else {
                        "other(length=$($_.Length))"
                    }
                }) -join ','
            $taskCandidates = @($afterPendingRename.Values | Where-Object {
                    (-not [string]::IsNullOrEmpty($_)) -and $_.EndsWith(
                        [IO.Path]::GetFileName($installedBinary),
                        [StringComparison]::OrdinalIgnoreCase
                    )
                }) -join '|'
            throw "self-uninstall did not append the expected delayed binary deletion entry; before_exists=$($beforePendingRename.Exists); before_count=$($beforePendingRename.Values.Count); after_exists=$($afterPendingRename.Exists); after_count=$($afterPendingRename.Values.Count); entry_shape=$entryShape; task_candidates=$taskCandidates"
        }
    }
    catch {
        $testFailure = $_
    }
    finally {
        # Identity probing and owned cleanup can fail after Test-Path succeeds. Keep the
        # installed path fail-closed until the exact task-owned binary is gone.
        $script:preserveInstalledBinary = $true
        if (Test-Path -LiteralPath $installedBinary -PathType Leaf) {
            try {
                $currentBinary = Get-Item -LiteralPath $installedBinary
                $currentBinaryHash = (Get-FileHash -LiteralPath $installedBinary -Algorithm SHA256).Hash
                $binaryUnchanged = ($currentBinary.Length -eq $ownedBinary.Length) -and
                    ($currentBinary.CreationTimeUtc.Ticks -eq $ownedBinary.CreationTimeUtc.Ticks) -and
                    ($currentBinary.LastWriteTimeUtc.Ticks -eq $ownedBinary.LastWriteTimeUtc.Ticks) -and
                    [string]::Equals($currentBinaryHash, $ownedBinaryHash, [StringComparison]::OrdinalIgnoreCase)
                if ($binaryUnchanged) {
                    Remove-Item -LiteralPath $installedBinary -Force
                    $removedOwnedBinary = -not (Test-Path -LiteralPath $installedBinary)
                    if ($removedOwnedBinary) {
                        $script:preserveInstalledBinary = $false
                    }
                    else {
                        $cleanupFailures.Add('self-uninstalled Agent binary still exists after owned cleanup')
                    }
                }
                else {
                    $cleanupFailures.Add('refusing to remove a self-uninstalled Agent binary whose identity changed')
                }
            }
            catch {
                $cleanupFailures.Add("inspect or remove self-uninstalled Agent binary failed: $($_.Exception.Message)")
            }
        }
        else {
            $cleanupFailures.Add('refusing to restore delayed deletion after the owned Agent binary disappeared')
        }

        if ($ownsPendingDelete) {
            if (-not $removedOwnedBinary) {
                $cleanupFailures.Add('refusing to restore delayed deletion without removing the owned Agent binary')
            }
            else {
                try {
                    Restore-PendingRenameSnapshot `
                        -Before $beforePendingRename -OwnedAfter $afterPendingRename
                }
                catch {
                    $cleanupFailures.Add("restore delayed deletion registry state failed: $($_.Exception.Message)")
                }
            }
        }
    }

    if ($null -ne $testFailure) {
        if ($cleanupFailures.Count -ne 0) {
            $cleanupMessage = $cleanupFailures -join [Environment]::NewLine
            throw "installed Agent self-uninstall failed: $($testFailure.Exception.Message)$([Environment]::NewLine)Cleanup also failed:$([Environment]::NewLine)$cleanupMessage"
        }
        throw $testFailure
    }
    if ($cleanupFailures.Count -ne 0) {
        throw ($cleanupFailures -join [Environment]::NewLine)
    }
}

function New-SmokeToken {
    # The Go protocol package owns the Connection Token wire format. Reuse its
    # production encoder instead of duplicating Protobuf, HMAC, or Base64URL here.
    $token = & go -C $repositoryRoot run ./deploy/smoketoken
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

function Assert-EventSourceContract {
    if (-not (Test-Path -LiteralPath $eventSourceRegistryPath)) {
        throw "Windows Event Log Source $eventSourceName is missing"
    }
    $source = Get-ItemProperty -LiteralPath $eventSourceRegistryPath
    if ($source.XTunnelManaged -ne $managedMarker) {
        throw "Windows Event Log Source $eventSourceName has an unexpected managed marker"
    }
    if ([int]$source.CustomSource -ne 1) {
        throw "Windows Event Log Source $eventSourceName is not a custom source"
    }
    if ([int]$source.TypesSupported -ne 7) {
        throw "Windows Event Log Source $eventSourceName has unexpected supported event types"
    }
}

function Assert-EventLogContract {
    param(
        [Parameter(Mandatory = $true)]
        [string[]]$ForbiddenValues,

        [int]$TimeoutSeconds = 30
    )

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    while ([DateTime]::UtcNow -lt $deadline) {
        $lifecycleFound = $false
        $records = @(Get-WinEvent -FilterHashtable @{
                LogName      = 'Application'
                ProviderName = $eventSourceName
                StartTime    = $eventQueryStart
            } -ErrorAction SilentlyContinue)
        foreach ($record in $records) {
            $message = $record.Message
            if ([string]::IsNullOrWhiteSpace($message)) {
                continue
            }
            foreach ($forbidden in $ForbiddenValues) {
                if ((-not [string]::IsNullOrEmpty($forbidden)) -and $message.Contains($forbidden)) {
                    throw 'Windows Event Log contains a plaintext Connection Token'
                }
            }
            try {
                $payload = $message | ConvertFrom-Json
            }
            catch {
                continue
            }
            $propertyNames = @($payload.PSObject.Properties.Name)
            foreach ($required in @('timestamp', 'level', 'component', 'event')) {
                if ($propertyNames -notcontains $required) {
                    throw "Windows Event Log JSON is missing required field $required"
                }
            }
            if (($payload.component -eq 'agent') -and
                ($payload.event -in @('windows_service_running', 'process_started'))) {
                $lifecycleFound = $true
            }
        }
        if ($lifecycleFound) {
            return
        }
        Start-Sleep -Milliseconds 250
    }
    throw 'Windows Event Log did not contain a valid Agent lifecycle JSON record'
}

function Wait-AgentEvent {
    param(
        [Parameter(Mandatory = $true)]
        [DateTime]$StartTime,

        [Parameter(Mandatory = $true)]
        [string]$Event,

        [string]$ErrorCode = '',

        [string[]]$ForbiddenValues = @(),

        [int]$TimeoutSeconds = 30
    )

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    while ([DateTime]::UtcNow -lt $deadline) {
        $records = @(Get-WinEvent -FilterHashtable @{
                LogName      = 'Application'
                ProviderName = $eventSourceName
                StartTime    = $StartTime
            } -ErrorAction SilentlyContinue)
        foreach ($record in $records) {
            $message = $record.Message
            if ([string]::IsNullOrWhiteSpace($message)) {
                continue
            }
            foreach ($forbidden in $ForbiddenValues) {
                if ((-not [string]::IsNullOrEmpty($forbidden)) -and $message.Contains($forbidden)) {
                    throw 'Windows Event Log contains a plaintext Connection Token'
                }
            }
            try {
                $payload = $message | ConvertFrom-Json
            }
            catch {
                continue
            }
            if (($payload.component -ne 'agent') -or ($payload.event -ne $Event)) {
                continue
            }
            if ((-not [string]::IsNullOrEmpty($ErrorCode)) -and ($payload.error_code -ne $ErrorCode)) {
                continue
            }
            return
        }
        Start-Sleep -Milliseconds 250
    }
    throw "Windows Event Log did not contain event=$Event error_code=$ErrorCode"
}

function Invoke-ScmFaultGate {
    param(
        [Parameter(Mandatory = $true)]
        [ValidateSet('runtime-failure', 'stop-timeout')]
        [string]$Mode,

        [Parameter(Mandatory = $true)]
        [string]$ErrorCode,

        [Parameter(Mandatory = $true)]
        [string[]]$ForbiddenValues,

        [switch]$RequestStop
    )

    $markerPath = Join-Path $gateDirectory "$Mode.marker"
    $helperCommand = '"' + $installedGateHelper + '" ' + $Mode + ' "' + $markerPath + '"'
    $gateFailure = $null
    $gateCleanupFailures = New-Object System.Collections.Generic.List[string]
    [uint32]$recoveredHelperProcessId = 0
    $commandRestored = $false

    try {
        if (Test-Path -LiteralPath $markerPath) {
            Remove-Item -LiteralPath $markerPath -Force
        }
        Stop-Service -Name $serviceName
        Wait-AgentServiceStatus -Status 'Stopped'
        Set-AgentServiceCommand -CommandLine $helperCommand

        $gateStart = [DateTime]::UtcNow.AddSeconds(-1)
        Start-Service -Name $serviceName
        Wait-AgentServiceStatus -Status 'Running'
        $firstHelper = Get-AgentService
        if (($null -eq $firstHelper) -or ($firstHelper.ProcessId -eq 0)) {
            throw "SCM fault helper for $Mode did not expose a running PID"
        }
        [uint32]$firstHelperProcessId = $firstHelper.ProcessId

        $stopStarted = $null
        if ($RequestStop) {
            $stopStarted = [DateTime]::UtcNow
            $output = & sc.exe stop $serviceName 2>&1
            if ($LASTEXITCODE -ne 0) {
                $message = ($output | ForEach-Object { $_.ToString() }) -join [Environment]::NewLine
                throw "sc.exe stop failed with exit code $LASTEXITCODE`: $message"
            }
        }

        $failureTimeout = if ($RequestStop) { 40 } else { 10 }
        $failedService = Wait-AgentServiceFailure -TimeoutSeconds $failureTimeout
        if (($null -eq $failedService) -or ($failedService.ExitCode -eq 0)) {
            throw "SCM fault helper for $Mode did not publish a non-zero service exit"
        }
        if ($RequestStop) {
            $stopElapsed = [DateTime]::UtcNow - $stopStarted
            if ($stopElapsed.TotalSeconds -lt 29) {
                throw "SCM Stop timeout returned after $($stopElapsed.TotalSeconds) seconds; want the production 30 second bound"
            }
            if ($stopElapsed.TotalSeconds -gt 35) {
                throw "SCM Stop timeout returned after $($stopElapsed.TotalSeconds) seconds; exceeded the 35 second Gate bound"
            }
        }
        Wait-AgentEvent -StartTime $gateStart -Event 'windows_service_failed' `
            -ErrorCode $ErrorCode -ForbiddenValues $ForbiddenValues -TimeoutSeconds $failureTimeout

        $recoveredHelper = Wait-AgentServiceProcessChange `
            -PreviousProcessId $firstHelperProcessId -TimeoutSeconds 15
        $recoveredHelperProcessId = $recoveredHelper.ProcessId
        Assert-AgentServiceStable
    }
    catch {
        $gateFailure = $_
    }
    finally {
        try {
            Set-AgentServiceCommand -CommandLine $expectedAgentCommand
            $commandRestored = $true
        }
        catch {
            $gateCleanupFailures.Add("restore production service command failed: $($_.Exception.Message)")
        }

        try {
            if (-not $commandRestored) {
                throw 'production service command was not restored'
            }
            $service = Get-AgentService
            if (($null -ne $service) -and ($service.State -ne 'Stopped')) {
                $output = & sc.exe stop $serviceName 2>&1
                if (($LASTEXITCODE -ne 0) -and ((Get-AgentService).State -ne 'Stopped')) {
                    $message = ($output | ForEach-Object { $_.ToString() }) -join [Environment]::NewLine
                    throw "sc.exe stop during restore failed with exit code $LASTEXITCODE`: $message"
                }
                Wait-AgentServiceStatus -Status 'Stopped' -TimeoutSeconds 40
            }
            Start-Service -Name $serviceName
            Wait-AgentServiceStatus -Status 'Running'
            Assert-AgentServiceStable
            $restoredService = Get-AgentService
            if (($recoveredHelperProcessId -ne 0) -and
                ($restoredService.ProcessId -eq $recoveredHelperProcessId)) {
                throw "restored Agent reused SCM fault helper PID $recoveredHelperProcessId"
            }
            Assert-ServiceContract
        }
        catch {
            $gateCleanupFailures.Add("restore production service process failed: $($_.Exception.Message)")
        }

        if (Test-Path -LiteralPath $markerPath) {
            try {
                Remove-Item -LiteralPath $markerPath -Force
            }
            catch {
                $gateCleanupFailures.Add("remove SCM fault marker failed: $($_.Exception.Message)")
            }
        }
    }

    if ($null -ne $gateFailure) {
        if ($gateCleanupFailures.Count -ne 0) {
            $cleanupMessage = $gateCleanupFailures -join [Environment]::NewLine
            throw "SCM fault gate $Mode failed: $($gateFailure.Exception.Message)$([Environment]::NewLine)Cleanup also failed:$([Environment]::NewLine)$cleanupMessage"
        }
        throw $gateFailure
    }
    if ($gateCleanupFailures.Count -ne 0) {
        throw ($gateCleanupFailures -join [Environment]::NewLine)
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
if (-not (Test-Path -LiteralPath $gateHelperFullPath -PathType Leaf)) {
    throw "Windows SCM Gate Helper not found: $gateHelperFullPath"
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
if (Test-Path -LiteralPath $eventSourceRegistryPath) {
    throw "refusing to overwrite existing Windows Event Log Source $eventSourceName"
}
if ((Test-Path -LiteralPath $installDirectory) -or (Test-Path -LiteralPath $productDataDirectory)) {
    throw 'refusing to overwrite an existing XTunnel install or data path'
}

try {
    $firstToken = New-SmokeToken
    Test-UnmanagedServiceBoundary -Token $firstToken
    Test-UnmanagedEventSourceBoundary -Token $firstToken
    $installAttempted = $true
    Invoke-Agent -ArgumentList @('service', 'install', '--token', $firstToken)
    Wait-AgentServiceStatus -Status 'Running'
    Assert-AgentServiceStable
    Assert-ServiceContract
    Assert-EventSourceContract
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
    Assert-EventSourceContract
    Assert-EventLogContract -ForbiddenValues @($firstToken, $secondToken)

    Stop-Service -Name $serviceName
    Wait-AgentServiceStatus -Status 'Stopped'
    Start-Service -Name $serviceName
    Wait-AgentServiceStatus -Status 'Running'
    Assert-AgentServiceStable

    # Corrupt only the isolated Runner credential while preserving its ACL. The service
    # must log a stable failure and exit non-zero before SCM recovery starts a new process.
    $recoveryProcessBefore = (Get-AgentService).ProcessId
    $validProtectedCredential = [IO.File]::ReadAllBytes($credentialPath)
    Stop-Service -Name $serviceName
    Wait-AgentServiceStatus -Status 'Stopped'
    $recoveryStart = [DateTime]::UtcNow.AddSeconds(-1)
    [IO.File]::WriteAllBytes($credentialPath, [byte[]](0x58, 0x54, 0x55, 0x4E, 0x4E, 0x45, 0x4C))
    try {
        Start-Service -Name $serviceName
    }
    catch {
        # SCM may observe the non-zero exit before Start-Service returns; Event Log is authoritative.
    }
    Wait-AgentEvent -StartTime $recoveryStart -Event 'windows_service_failed' `
        -ErrorCode 'CREDENTIAL_LOAD_FAILED' -ForbiddenValues @($firstToken, $secondToken)
    $credentialFailure = Wait-AgentServiceFailure
    if (($null -eq $credentialFailure) -or ($credentialFailure.ExitCode -eq 0)) {
        throw 'credential failure did not publish a non-zero SCM exit'
    }
    [IO.File]::WriteAllBytes($credentialPath, $validProtectedCredential)
    Wait-AgentServiceStatus -Status 'Running'
    Assert-AgentServiceStable
    $recoveryProcessAfter = (Get-AgentService).ProcessId
    if (($recoveryProcessAfter -eq 0) -or ($recoveryProcessAfter -eq $recoveryProcessBefore)) {
        throw "SCM recovery did not start a new Agent process: before=$recoveryProcessBefore after=$recoveryProcessAfter"
    }
    Wait-AgentEvent -StartTime $recoveryStart -Event 'windows_service_running' `
        -ForbiddenValues @($firstToken, $secondToken)
    Assert-CredentialProtected -PlaintextToken $secondToken

    New-Item -ItemType Directory -Path $gateDirectory | Out-Null
    & icacls.exe $gateDirectory /inheritance:r `
        /grant:r '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' '*S-1-5-19:(OI)(CI)M' | Out-Null
    if ($LASTEXITCODE -ne 0) {
        throw "failed to protect Windows SCM Gate directory (exit code $LASTEXITCODE)"
    }
    Copy-Item -LiteralPath $gateHelperFullPath -Destination $installedGateHelper

    Invoke-ScmFaultGate -Mode 'runtime-failure' -ErrorCode 'RUNTIME_FAILED' `
        -ForbiddenValues @($firstToken, $secondToken)
    Invoke-ScmFaultGate -Mode 'stop-timeout' -ErrorCode 'STOP_TIMEOUT' `
        -ForbiddenValues @($firstToken, $secondToken) -RequestStop
    Assert-EventLogContract -ForbiddenValues @($firstToken, $secondToken)

    $installedBinaryHashAfterGates = (Get-FileHash -LiteralPath $installedBinary -Algorithm SHA256).Hash
    if ($sourceBinaryHash -ne $installedBinaryHashAfterGates) {
        throw 'SCM fault gates modified the installed production Agent binary'
    }

    Remove-Item -LiteralPath $installedGateHelper -Force
    Remove-EmptyDirectory -LiteralPath $gateDirectory

    Invoke-InstalledAgentSelfUninstall
    $uninstallCompleted = $true
    Wait-AgentServiceDeleted
    if (Test-Path -LiteralPath $installedBinary) {
        throw 'installed Agent binary still exists after uninstall'
    }
    if (-not (Test-Path -LiteralPath $credentialPath -PathType Leaf)) {
        throw 'uninstall must preserve the protected Agent credential'
    }
    if (Test-Path -LiteralPath $eventSourceRegistryPath) {
        throw 'managed Windows Event Log Source still exists after uninstall'
    }
}
catch {
    $primaryFailure = $_
}
finally {
    $remainingServiceBeforeUninstall = Get-AgentService
    if (($null -ne $remainingServiceBeforeUninstall) -and
        ($remainingServiceBeforeUninstall.PathName -match ([Regex]::Escape($installedGateHelper)))) {
        if ($preserveInstalledBinary) {
            $cleanupFailures.Add('refusing to restore a service command after the installed Agent binary ownership changed')
        }
        else {
            try {
                Set-AgentServiceCommand -CommandLine $expectedAgentCommand
            }
            catch {
                $cleanupFailures.Add("restore service command before uninstall failed: $($_.Exception.Message)")
            }
        }
    }

    if ($installAttempted -and (-not $uninstallCompleted) -and (-not $preserveInstalledBinary)) {
        try {
            Invoke-Agent -ArgumentList @('service', 'uninstall')
            $uninstallCompleted = $true
        }
        catch {
            $cleanupFailures.Add("managed uninstall failed: $($_.Exception.Message)")
        }
    }

    $remainingService = Get-AgentService
    if (($null -ne $remainingService) -and $preserveInstalledBinary) {
        $cleanupFailures.Add('refusing to remove a service after the installed Agent binary ownership changed')
        $remainingService = $null
    }
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

    if ((Test-Path -LiteralPath $eventSourceRegistryPath) -and $preserveInstalledBinary) {
        $cleanupFailures.Add('refusing to remove an Event Log Source after the installed Agent binary ownership changed')
    }
    elseif (Test-Path -LiteralPath $eventSourceRegistryPath) {
        try {
            $source = Get-ItemProperty -LiteralPath $eventSourceRegistryPath
            if ($installAttempted -and ($source.XTunnelManaged -eq $managedMarker)) {
                Remove-Item -LiteralPath $eventSourceRegistryPath -Force
            }
            else {
                $cleanupFailures.Add('refusing to remove an unmanaged Windows Event Log Source')
            }
        }
        catch {
            $cleanupFailures.Add("Event Log Source cleanup failed: $($_.Exception.Message)")
        }
    }

    foreach ($path in @($installedGateHelper, $installedBinary, $credentialPath)) {
        if ($preserveInstalledBinary -and (@($installedBinary, $credentialPath) -contains $path)) {
            $cleanupFailures.Add("refusing to remove an install artifact after ownership changed: $path")
            continue
        }
        if (Test-Path -LiteralPath $path -PathType Leaf) {
            try {
                Remove-Item -LiteralPath $path -Force
            }
            catch {
                $cleanupFailures.Add("file cleanup failed for $path`: $($_.Exception.Message)")
            }
        }
    }
    foreach ($directory in @($gateDirectory, $credentialDirectory, $productDataDirectory, $installDirectory)) {
        if ($preserveInstalledBinary -and ($directory -ne $gateDirectory)) {
            $cleanupFailures.Add("refusing to remove an install directory after ownership changed: $directory")
            continue
        }
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
