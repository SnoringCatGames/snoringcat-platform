#!/usr/bin/env pwsh
# Calls the Nakama runtime's `runtime_status` RPC and pretty-
# prints the JSON response. Use this to verify which build is
# actually loaded on the live server, what Edgegap config it saw
# at init time, and which RPCs/hooks it registered.
#
# `runtime_status` is gated to server-to-server callers (HTTP
# key only). Pass the Nakama HTTP key via -HttpKey or set the
# NAKAMA_HTTP_KEY environment variable. The expected value lives
# in your local credentials.env (it matches the value Nakama is
# started with via --runtime.http_key in
# infra/remote/nakama/docker-compose.yml).
#
# Usage:
#   pwsh scripts/probe-runtime-status.ps1
#   pwsh scripts/probe-runtime-status.ps1 -HttpKey 'xxxxx'
#   pwsh scripts/probe-runtime-status.ps1 -NakamaHost 'https://staging.example.com'

[CmdletBinding()]
param(
    [string]$NakamaHost = "https://nakama.snoringcat.games",
    [string]$HttpKey = $env:NAKAMA_HTTP_KEY
)

$ErrorActionPreference = "Stop"

if (-not $HttpKey) {
    Write-Error @"
NAKAMA_HTTP_KEY is required. Either:
  - Set the env var:  `$env:NAKAMA_HTTP_KEY = '...'`
  - Pass it inline:   pwsh scripts/probe-runtime-status.ps1 -HttpKey '...'

The key is the same value you set on `--runtime.http_key` when
starting Nakama (see infra/remote/nakama/docker-compose.yml).
"@
    exit 2
}

# RPC takes no payload; URL-encoded empty body matches the wire
# format Nakama expects when called via HTTP key.
$encodedKey = [Uri]::EscapeDataString($HttpKey)
$uri = "$NakamaHost/v2/rpc/runtime_status?http_key=$encodedKey&unwrap=true"

# Redact the ENCODED key (what actually appears in $uri). Redacting
# the raw key fails to match once it contains characters that get
# percent-encoded (a base64 key has /, +, =), which silently leaked
# the key in cleartext.
Write-Host "POST $($uri.Replace($encodedKey, '<redacted>'))"
try {
    $payload = Invoke-RestMethod `
        -Method POST `
        -Uri $uri `
        -ContentType "application/json" `
        -Body '""'
} catch {
    Write-Host "RPC call failed: $($_.Exception.Message)"
    if ($_.Exception.Response) {
        $errStream = $_.Exception.Response.GetResponseStream()
        $errReader = [System.IO.StreamReader]::new($errStream)
        $errBody = $errReader.ReadToEnd()
        Write-Host "Response body: $errBody"

        # Common failure modes, with hints.
        if ($errBody -match "function not found") {
            Write-Host ""
            Write-Host "Diagnosis: the runtime plugin doesn't have"
            Write-Host "runtime_status registered. Either it predates"
            Write-Host "this RPC (deploy a fresh build via release"
            Write-Host "tag or workflow_dispatch) or the plugin"
            Write-Host "failed to load entirely (check Nakama server"
            Write-Host "logs)."
        } elseif ($errBody -match "invalid http key") {
            Write-Host ""
            Write-Host "Diagnosis: HTTP key mismatch. Verify"
            Write-Host "NAKAMA_HTTP_KEY matches what the live Nakama"
            Write-Host "container was started with."
        } elseif ($errBody -match "forbidden") {
            Write-Host ""
            Write-Host "Diagnosis: deployed runtime is gating this"
            Write-Host "RPC differently than expected. Inspect Nakama"
            Write-Host "logs and the runtime version."
        }
    }
    exit 1
}

Write-Host ""
Write-Host "=== runtime_status ==="
$payload | ConvertTo-Json -Depth 5
