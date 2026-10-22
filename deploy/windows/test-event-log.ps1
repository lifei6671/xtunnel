[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot 'event-log.ps1')

$json = '{"timestamp":"2026-10-22T00:00:00Z","level":"info","component":"agent","event":"windows_service_running"}'
foreach ($message in @($null, '', 'The description for Event ID 1 cannot be found.', "localized prefix: $json")) {
    $record = [pscustomobject]@{ Message = $message; Properties = @([pscustomobject]@{ Value = $json }) }
    $payload = ConvertFrom-AgentEventLogRecord -Record $record
    if (($payload.component -ne 'agent') -or ($payload.event -ne 'windows_service_running')) {
        throw 'Raw lifecycle JSON was not preserved'
    }
}

# 检查字段、非法 JSON 和明文 Token 都必须继续失败，且错误不能回显秘密。
$secret = 'fixture-token-must-remain-private'
$cases = @(
    @{ Values = @(); Expected = 'exactly one JSON' },
    @{ Values = @($json, $json); Expected = 'exactly one JSON' },
    @{ Values = @('{invalid-json'); Expected = 'not valid JSON' },
    @{ Values = @('null'); Expected = 'must be an object' },
    @{ Values = @('[]'); Expected = 'must be an object' },
    @{ Values = @("[$json]"); Expected = 'must be an object' },
    @{ Values = @('{"level":"info","component":"agent","event":"process_started"}'); Expected = 'required field timestamp' },
    @{ Values = @($json.Replace('windows_service_running', '')); Expected = 'required field event' },
    @{ Values = @($json.Replace('windows_service_running', $secret)); Expected = 'plaintext Connection Token' },
    @{ Values = @($json, $secret); Expected = 'plaintext Connection Token' }
)
foreach ($case in $cases) {
    $record = [pscustomobject]@{
        Message = $json
        Properties = @($case.Values | ForEach-Object { [pscustomobject]@{ Value = $_ } })
    }
    $failure = $null
    try { $null = ConvertFrom-AgentEventLogRecord -Record $record -ForbiddenValues @($secret) }
    catch { $failure = $_.Exception.Message }
    if (($null -eq $failure) -or (-not $failure.Contains($case.Expected)) -or $failure.Contains($secret)) {
        throw "Event Log contract regression failed: $($case.Expected)"
    }
}
# 使用局部替身检查查询边界，不依赖本机安装 Agent 或事件源元数据。
function Get-WinEvent {
    [CmdletBinding()]
    param([string]$LogName, [string]$FilterXPath)
    if (($LogName -ne 'Application') -or
        ($FilterXPath -ne "*[System[Provider[@Name='XTunnelAgent'] and TimeCreated[@SystemTime>='2026-10-22T00:00:00.0000000Z']]]")) {
        throw 'Event query lost its source or UTC time boundary'
    }
    if ($script:queryErrorID) {
        Write-Error -Message 'fixture query failure' -ErrorId $script:queryErrorID
    }
    else {
        [pscustomobject]@{ RecordId = 42 }
    }
}
$queryStart = [DateTimeOffset]::Parse('2026-10-22T08:00:00+08:00').UtcDateTime
$script:queryErrorID = ''
if ((Get-AgentEventLogRecords -StartTime $queryStart).RecordId -ne 42) {
    throw 'Event query did not preserve its result'
}
$script:queryErrorID = 'NoMatchingEventsFound'
if (@(Get-AgentEventLogRecords -StartTime $queryStart).Count -ne 0) {
    throw 'Empty event query must return no records'
}
$script:queryErrorID = 'EventLogAccessDenied'
$failure = $null
try { $null = Get-AgentEventLogRecords -StartTime $queryStart }
catch { $failure = $_.Exception.Message }
if (($null -eq $failure) -or (-not $failure.Contains('EventLogAccessDenied'))) {
    throw 'Event query suppressed an unexpected failure'
}
Write-Host 'Agent Event Log query and raw-record contract tests passed'
