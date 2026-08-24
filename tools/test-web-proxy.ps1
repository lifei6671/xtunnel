[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$repoRoot = Split-Path -Parent $scriptDir
$webDir = Join-Path $repoRoot "web"
$viteEntry = Join-Path $webDir "node_modules\vite\bin\vite.js"
$tempRoot = Join-Path $repoRoot ".tools\web-proxy-test.$([guid]::NewGuid().ToString('N'))"
$backendJob = $null
$viteProcess = $null
$missingCertProcess = $null
$missingKeyProcess = $null
$previousCertEnv = [System.Environment]::GetEnvironmentVariable(
    "XTUNNEL_DEV_TLS_CERT",
    [System.EnvironmentVariableTarget]::Process
)
$previousKeyEnv = [System.Environment]::GetEnvironmentVariable(
    "XTUNNEL_DEV_TLS_KEY",
    [System.EnvironmentVariableTarget]::Process
)

try {
    if (-not (Test-Path -LiteralPath $viteEntry)) {
        throw "missing Vite dependency; run npm ci in web first"
    }

    New-Item -ItemType Directory -Path $tempRoot | Out-Null
    $nodePath = (Get-Command node).Source
    $missingCertStdoutPath = Join-Path $tempRoot "missing-cert.stdout.log"
    $missingCertStderrPath = Join-Path $tempRoot "missing-cert.stderr.log"

    # A missing loopback certificate must fail before Vite can expose an HTTP fallback.
    Remove-Item Env:XTUNNEL_DEV_TLS_CERT -ErrorAction SilentlyContinue
    Remove-Item Env:XTUNNEL_DEV_TLS_KEY -ErrorAction SilentlyContinue
    $missingCertProcess = Start-Process -FilePath $nodePath `
        -ArgumentList @($viteEntry, "--clearScreen", "false") `
        -WorkingDirectory $webDir `
        -WindowStyle Hidden `
        -RedirectStandardOutput $missingCertStdoutPath `
        -RedirectStandardError $missingCertStderrPath `
        -PassThru
    if (-not $missingCertProcess.WaitForExit(5000)) {
        Stop-Process -Id $missingCertProcess.Id -Force
        $missingCertProcess.WaitForExit()
        throw "Vite stayed running without a development certificate"
    }
    $missingCertOutput = (Get-Content -Raw -LiteralPath $missingCertStdoutPath) +
        (Get-Content -Raw -LiteralPath $missingCertStderrPath)
    if ($missingCertProcess.ExitCode -eq 0) {
        throw "Vite accepted a missing development certificate"
    }
    if ($missingCertOutput -notmatch "XTUNNEL_DEV_TLS_CERT is required") {
        throw "Vite did not report the required development certificate variable"
    }

    $certPath = Join-Path $tempRoot "loopback-cert.pem"
    $keyPath = Join-Path $tempRoot "loopback-key.pem"

    # Generate a one-run loopback certificate without touching the user certificate store.
    $rsa = [System.Security.Cryptography.RSA]::Create(2048)
    $request = [System.Security.Cryptography.X509Certificates.CertificateRequest]::new(
        "CN=127.0.0.1",
        $rsa,
        [System.Security.Cryptography.HashAlgorithmName]::SHA256,
        [System.Security.Cryptography.RSASignaturePadding]::Pkcs1
    )
    $san = [System.Security.Cryptography.X509Certificates.SubjectAlternativeNameBuilder]::new()
    $san.AddIpAddress([System.Net.IPAddress]::Loopback)
    $request.CertificateExtensions.Add($san.Build())
    $now = [System.DateTimeOffset]::UtcNow
    $certificate = $request.CreateSelfSigned($now.AddMinutes(-1), $now.AddDays(1))
    [System.IO.File]::WriteAllText($certPath, $certificate.ExportCertificatePem())
    [System.IO.File]::WriteAllText($keyPath, $rsa.ExportPkcs8PrivateKeyPem())

    $missingKeyStdoutPath = Join-Path $tempRoot "missing-key.stdout.log"
    $missingKeyStderrPath = Join-Path $tempRoot "missing-key.stderr.log"
    $env:XTUNNEL_DEV_TLS_CERT = $certPath
    Remove-Item Env:XTUNNEL_DEV_TLS_KEY -ErrorAction SilentlyContinue
    $missingKeyProcess = Start-Process -FilePath $nodePath `
        -ArgumentList @($viteEntry, "--clearScreen", "false") `
        -WorkingDirectory $webDir `
        -WindowStyle Hidden `
        -RedirectStandardOutput $missingKeyStdoutPath `
        -RedirectStandardError $missingKeyStderrPath `
        -PassThru
    if (-not $missingKeyProcess.WaitForExit(5000)) {
        Stop-Process -Id $missingKeyProcess.Id -Force
        $missingKeyProcess.WaitForExit()
        throw "Vite stayed running without a development certificate key"
    }
    $missingKeyOutput = (Get-Content -Raw -LiteralPath $missingKeyStdoutPath) +
        (Get-Content -Raw -LiteralPath $missingKeyStderrPath)
    if ($missingKeyProcess.ExitCode -eq 0) {
        throw "Vite accepted a missing development certificate key"
    }
    if ($missingKeyOutput -notmatch "XTUNNEL_DEV_TLS_KEY is required") {
        throw "Vite did not report the required development certificate key variable"
    }

    # The fixture returns the headers it actually receives through Vite's proxy.
    $backendJob = Start-Job -ScriptBlock {
        $listener = [System.Net.HttpListener]::new()
        $listener.Prefixes.Add("http://127.0.0.1:8080/")
        try {
            $listener.Start()
            $context = $listener.GetContext()
            $payload = @{
                host = $context.Request.Headers["Host"]
                origin = $context.Request.Headers["Origin"]
                path = $context.Request.RawUrl
            } | ConvertTo-Json -Compress
            $bytes = [System.Text.Encoding]::UTF8.GetBytes($payload)
            $context.Response.ContentType = "application/json"
            $context.Response.ContentLength64 = $bytes.Length
            $context.Response.OutputStream.Write($bytes, 0, $bytes.Length)
            $context.Response.Close()
        }
        finally {
            $listener.Close()
        }
    }

    $env:XTUNNEL_DEV_TLS_CERT = $certPath
    $env:XTUNNEL_DEV_TLS_KEY = $keyPath
    $stdoutPath = Join-Path $tempRoot "vite.stdout.log"
    $stderrPath = Join-Path $tempRoot "vite.stderr.log"
    $viteProcess = Start-Process -FilePath $nodePath `
        -ArgumentList @($viteEntry, "--clearScreen", "false") `
        -WorkingDirectory $webDir `
        -WindowStyle Hidden `
        -RedirectStandardOutput $stdoutPath `
        -RedirectStandardError $stderrPath `
        -PassThru

    $response = $null
    for ($attempt = 0; $attempt -lt 50; $attempt++) {
        if ($viteProcess.HasExited) {
            $stderr = Get-Content -Raw -LiteralPath $stderrPath
            throw "Vite exited before proxy smoke: $stderr"
        }
        try {
            $response = Invoke-RestMethod `
                -Uri "https://127.0.0.1:5173/api/v1/proxy-smoke" `
                -Headers @{ Origin = "https://127.0.0.1:5173" } `
                -SkipCertificateCheck
            break
        }
        catch {
            Start-Sleep -Milliseconds 100
        }
    }
    if ($null -eq $response) {
        throw "Vite proxy did not become ready"
    }
    if ($response.host -ne "127.0.0.1:5173") {
        throw "proxy changed Host: $($response.host)"
    }
    if ($response.origin -ne "https://127.0.0.1:5173") {
        throw "proxy changed Origin: $($response.origin)"
    }
    if ($response.path -ne "/api/v1/proxy-smoke") {
        throw "proxy changed request path: $($response.path)"
    }

    Write-Output "Web HTTPS proxy smoke passed."
}
finally {
    if (($null -ne $missingCertProcess) -and (-not $missingCertProcess.HasExited)) {
        Stop-Process -Id $missingCertProcess.Id -Force
        $missingCertProcess.WaitForExit()
    }
    if (($null -ne $missingKeyProcess) -and (-not $missingKeyProcess.HasExited)) {
        Stop-Process -Id $missingKeyProcess.Id -Force
        $missingKeyProcess.WaitForExit()
    }
    if (($null -ne $viteProcess) -and (-not $viteProcess.HasExited)) {
        Stop-Process -Id $viteProcess.Id -Force
        $viteProcess.WaitForExit()
    }
    if ($null -ne $backendJob) {
        Stop-Job -Job $backendJob -ErrorAction SilentlyContinue
        Remove-Job -Job $backendJob -Force -ErrorAction SilentlyContinue
    }
    if (Test-Path -LiteralPath $tempRoot) {
        Remove-Item -LiteralPath $tempRoot -Recurse -Force
    }
    if ($null -eq $previousCertEnv) {
        Remove-Item Env:XTUNNEL_DEV_TLS_CERT -ErrorAction SilentlyContinue
    }
    else {
        $env:XTUNNEL_DEV_TLS_CERT = $previousCertEnv
    }
    if ($null -eq $previousKeyEnv) {
        Remove-Item Env:XTUNNEL_DEV_TLS_KEY -ErrorAction SilentlyContinue
    }
    else {
        $env:XTUNNEL_DEV_TLS_KEY = $previousKeyEnv
    }
}
