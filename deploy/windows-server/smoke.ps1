[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$ServerPath,
    [Parameter(Mandatory = $true)][string]$ConfigPath
)

# 仅用于一次性提升权限 Windows amd64 Runner；入口先拒绝所有既有 Server 对象。
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$binary = [IO.Path]::GetFullPath($ServerPath)
$configSource = [IO.Path]::GetFullPath($ConfigPath)
$installedBinary = Join-Path $env:ProgramFiles 'XTunnel\xtunnel-server.exe'
$installedConfig = Join-Path $env:ProgramData 'XTunnel\server.yaml'
$dataRoot = Join-Path $env:ProgramData 'XTunnel\Server'
$eventKey = 'HKLM:\SYSTEM\CurrentControlSet\Services\EventLog\Application\XTunnelServer'
$eventStart = [DateTime]::Now.AddSeconds(-1)
if (-not ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { throw 'An elevated Administrator runner is required' }
if ([Runtime.InteropServices.RuntimeInformation]::OSArchitecture -ne 'X64') { throw 'Windows amd64 is required' }
if ((Get-Service XTunnelServer -ErrorAction SilentlyContinue) -or (Test-Path -LiteralPath $installedBinary) -or (Test-Path -LiteralPath $installedConfig) -or (Test-Path -LiteralPath $dataRoot) -or (Test-Path -LiteralPath $eventKey)) { throw 'Runner contains existing XTunnelServer objects; refusing to modify them' }

function Invoke-Server([string[]]$Arguments) {
    & $binary @Arguments
    if ($LASTEXITCODE -ne 0) {
        $commandExit = $LASTEXITCODE
        # 服务失败时保留 SCM 状态和已有结构化事件，便于隔离 Runner 定位启动阶段。
        & sc.exe queryex XTunnelServer
        $events = @(Get-WinEvent -FilterHashtable @{LogName='Application'; ProviderName='XTunnelServer'; StartTime=$eventStart} -MaxEvents 20 -ErrorAction SilentlyContinue)
        foreach ($entry in $events) {
            foreach ($property in $entry.Properties) { Write-Output $property.Value }
        }
        throw "Server command failed with exit $commandExit"
    }
}
function Wait-State([string]$State) {
    $deadline = [DateTime]::UtcNow.AddSeconds(35)
    while ([DateTime]::UtcNow -lt $deadline) {
        if ((Get-Service XTunnelServer).Status.ToString() -eq $State) { return }
        Start-Sleep -Milliseconds 200
    }
    throw "SCM did not reach $State within 35 seconds"
}

# 无效配置必须在 SCM、文件与 Event Source 创建前失败。
$invalid = Join-Path $env:RUNNER_TEMP 'xtunnel-server-invalid.yaml'
[IO.File]::WriteAllText($invalid, 'invalid_field: true')
& $binary service install --config $invalid
if ($LASTEXITCODE -eq 0) { throw 'Invalid configuration accepted' }
foreach ($path in @($installedBinary, $installedConfig, $dataRoot, $eventKey)) { if (Test-Path -LiteralPath $path) { throw "Invalid configuration mutated $path" } }
if (Get-Service XTunnelServer -ErrorAction SilentlyContinue) { throw 'Invalid configuration created SCM service' }

# 精确同名但无受管 Marker 的 SCM 对象必须原样拒绝。
& sc.exe create XTunnelServer binPath= $binary start= demand | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'Unable to create isolated foreign SCM fixture' }
try {
    & $binary service install --config $configSource
    if ($LASTEXITCODE -eq 0) { throw 'Unmanaged SCM fixture accepted' }
    & $binary service uninstall
    if ($LASTEXITCODE -eq 0) { throw 'Unmanaged SCM fixture deleted' }
    if (-not (Get-Service XTunnelServer -ErrorAction SilentlyContinue)) { throw 'Foreign SCM fixture disappeared' }
    foreach ($path in @($installedBinary, $installedConfig, $dataRoot, $eventKey)) { if (Test-Path -LiteralPath $path) { throw "Foreign SCM rejection mutated $path" } }
} finally {
    & sc.exe delete XTunnelServer | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'Unable to delete isolated SCM fixture' }
}

Invoke-Server @('service', 'install', '--config', $configSource)
Wait-State 'Running'
$instance = Get-CimInstance Win32_Service -Filter "Name='XTunnelServer'"
if ($instance.StartName -ne 'NT AUTHORITY\LocalService') { throw 'Unexpected service identity' }
if ($instance.PathName -notmatch ' --config ' -or $instance.PathName -match ' --set ') { throw 'Unexpected ImagePath' }
$configHash = (Get-FileHash -LiteralPath $installedConfig -Algorithm SHA256).Hash
$stopwatch = [Diagnostics.Stopwatch]::StartNew()
Stop-Service XTunnelServer
Wait-State 'Stopped'
$stopwatch.Stop()
if ($stopwatch.Elapsed.TotalSeconds -gt 35) { throw 'SCM stop exceeded convergence allowance' }
$passwordPath = Join-Path $env:RUNNER_TEMP ('xtunnel-server-password-' + [Guid]::NewGuid().ToString('N'))
$random = [Security.Cryptography.RandomNumberGenerator]::Create()
$passwordBytes = New-Object byte[] 32
try { $random.GetBytes($passwordBytes) } finally { $random.Dispose() }
$password = [Convert]::ToBase64String($passwordBytes)
[Array]::Clear($passwordBytes, 0, $passwordBytes.Length)
try {
    [IO.File]::WriteAllText($passwordPath, $password)
    Invoke-Server @('admin', 'create', '--config', $installedConfig, '--username', 'scm-smoke-admin', '--password-file', $passwordPath)
} finally {
    $password = $null
    if (Test-Path -LiteralPath $passwordPath) { Remove-Item -LiteralPath $passwordPath -Force }
}
Start-Service XTunnelServer
Wait-State 'Running'
# 示例配置对应三个业务入口；仅连接已冻结的 Loopback 测试端口。
foreach ($port in @(8080, 8081, 7443)) {
    $client = [Net.Sockets.TcpClient]::new()
    try {
        if (-not $client.ConnectAsync('127.0.0.1', $port).Wait([TimeSpan]::FromSeconds(5))) {
            throw "Business listener $port did not accept within five seconds"
        }
    }
    finally { $client.Dispose() }
}
Stop-Service XTunnelServer
Wait-State 'Stopped'

# 人工维护窗口中制造配置加载失败，确认 non-crash 恢复两次后停止。
$original = [IO.File]::ReadAllBytes($installedConfig)
[IO.File]::WriteAllText($installedConfig, 'invalid_field: true')
$recoveryStart = [DateTime]::Now.AddSeconds(-1)
try {
    & sc.exe start XTunnelServer | Out-Null
    Start-Sleep -Seconds 18
    Wait-State 'Stopped'
    $failures = @(Get-WinEvent -FilterHashtable @{LogName='Application';ProviderName='XTunnelServer';StartTime=$recoveryStart} | Where-Object { $_.Message -match 'windows_service_starting' })
    if ($failures.Count -ne 3) { throw "Expected initial start and two bounded retries; got $($failures.Count)" }
    Start-Sleep -Seconds 6
    $later = @(Get-WinEvent -FilterHashtable @{LogName='Application';ProviderName='XTunnelServer';StartTime=$recoveryStart} | Where-Object { $_.Message -match 'windows_service_starting' })
    if ($later.Count -ne 3) { throw 'Recovery continued after final NoAction' }
} finally { [IO.File]::WriteAllBytes($installedConfig, $original) }
Start-Service XTunnelServer
Wait-State 'Running'
$events = @(Get-WinEvent -FilterHashtable @{LogName='Application';ProviderName='XTunnelServer';StartTime=$eventStart})
if (-not ($events | Where-Object { $_.Message -match 'windows_service_running' })) { throw 'Missing running Event Log record' }
foreach ($entry in $events) {
    $json = $entry.Properties[0].Value | ConvertFrom-Json
    if (-not $json.level -or -not $json.event -or $json.component -ne 'server') { throw 'Event Log JSON contract is missing stable fields' }
}

# 外部部署副本卸载，保留 Config、Data、Runtime 和 ACL；同名重装恢复 SID 访问。
Stop-Service XTunnelServer
Wait-State 'Stopped'
$preserved = @($installedConfig, (Join-Path $dataRoot 'data'), (Join-Path $dataRoot 'runtime'))
$securityBefore = @{}
foreach ($path in $preserved) { $securityBefore[$path] = (Get-Acl -LiteralPath $path).Sddl }
$database = Join-Path $dataRoot 'data\xtunnel.db'
$databaseHash = (Get-FileHash -LiteralPath $database -Algorithm SHA256).Hash
Invoke-Server @('service', 'uninstall')
if (Get-Service XTunnelServer -ErrorAction SilentlyContinue) { throw 'SCM service remains after uninstall' }
if ((Test-Path -LiteralPath $installedBinary) -or (Test-Path -LiteralPath $eventKey)) { throw 'Managed binary/Event Source remains after uninstall' }
if (-not (Test-Path -LiteralPath $dataRoot) -or (Get-FileHash -LiteralPath $installedConfig -Algorithm SHA256).Hash -ne $configHash) { throw 'Uninstall modified persistent state' }
foreach ($path in $preserved) { if ((Get-Acl -LiteralPath $path).Sddl -ne $securityBefore[$path]) { throw "Uninstall modified security descriptor for $path" } }
if ((Get-FileHash -LiteralPath $database -Algorithm SHA256).Hash -ne $databaseHash) { throw 'Uninstall changed stopped database contents' }
Invoke-Server @('service', 'install', '--config', $configSource)
Wait-State 'Running'
Invoke-Server @('service', 'uninstall')
Write-Output 'XTunnelServer SCM smoke passed: install/readiness/stop/restart/bounded non-crash recovery/Event Log/uninstall/reinstall preservation'
