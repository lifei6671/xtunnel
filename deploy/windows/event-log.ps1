function Get-AgentEventLogRecords {
    param([Parameter(Mandatory = $true)][DateTime]$StartTime)

    # ProviderName Hashtable 会先解析 Provider 元数据；经典 Event Source 的
    # 消息资源在不同架构上未必可加载。直接按事件 XML 的来源和 UTC 时间查询。
    $since = $StartTime.ToUniversalTime().ToString('o', [Globalization.CultureInfo]::InvariantCulture)
    $query = "*[System[Provider[@Name='XTunnelAgent'] and TimeCreated[@SystemTime>='$since']]]"
    try {
        Get-WinEvent -LogName 'Application' -FilterXPath $query -ErrorAction Stop
    }
    catch {
        if ($_.FullyQualifiedErrorId -like 'NoMatchingEventsFound,*') {
            return
        }
        # 只有“尚无事件”允许轮询；权限、查询或读取故障必须保留明确失败。
        throw "Windows Event Log query failed (error_id=$($_.FullyQualifiedErrorId))"
    }
}

# Event Log 的 Message 是依赖系统消息资源和语言的展示文本；ReportEvent 写入的
# JSON 位于唯一的 insertion string。直接验证原始属性，保证平台差异不影响契约检查。
function ConvertFrom-AgentEventLogRecord {
    param(
        [Parameter(Mandatory = $true)]
        [object]$Record,

        [string[]]$ForbiddenValues = @()
    )

    $properties = @($Record.Properties)
    foreach ($property in $properties) {
        $value = [string]$property.Value
        foreach ($forbidden in $ForbiddenValues) {
            if ((-not [string]::IsNullOrEmpty($forbidden)) -and $value.Contains($forbidden)) {
                throw 'Windows Event Log contains a plaintext Connection Token'
            }
        }
    }
    if (($properties.Count -ne 1) -or ($properties[0].Value -isnot [string])) {
        throw 'Windows Event Log must contain exactly one JSON insertion string'
    }
    # PowerShell 5.1 与 7 对顶层 null/数组的转换不同；先限定日志的对象形状，
    # 再让 JSON 解析器校验完整语法，避免单元素数组被管道自动展开为日志对象。
    if (-not $properties[0].Value.TrimStart().StartsWith('{')) {
        throw 'Windows Event Log JSON must be an object'
    }
    try {
        $payload = $properties[0].Value | ConvertFrom-Json -ErrorAction Stop
    }
    catch {
        # 解析异常可能含原始载荷，因此只返回固定诊断。
        throw 'Windows Event Log insertion string is not valid JSON'
    }
    if (($null -eq $payload) -or ($payload -isnot [pscustomobject])) {
        throw 'Windows Event Log JSON must be an object'
    }
    $propertyNames = @($payload.PSObject.Properties.Name)
    foreach ($required in @('timestamp', 'level', 'component', 'event')) {
        if (($propertyNames -notcontains $required) -or
            [string]::IsNullOrWhiteSpace([string]$payload.$required)) {
            throw "Windows Event Log JSON is missing required field $required"
        }
    }
    return $payload
}
