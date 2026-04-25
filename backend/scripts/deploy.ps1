# Backend Deployment Script
# Usage: .\scripts\deploy-backend.ps1
#
# Syncs GAME_VERSION in template.yaml from project.godot,
# then runs sam build and sam deploy.
#
# Prerequisites:
#   - AWS CLI configured (aws sso login --profile hopnbop)
#   - AWS SAM CLI installed
#   - Python 3.12

param(
    [string]$Profile = "hopnbop",
    [string]$Region = "us-west-2"
)

$ErrorActionPreference = "Stop"

Write-Host "=== Backend Deployment ===" -ForegroundColor Cyan

# Read version from project.godot (single source of truth).
$projectGodot = Get-Content "project.godot" -Raw
if ($projectGodot -match 'config/version="([^"]+)"') {
    $Version = $Matches[1]
} else {
    Write-Error "Could not read config/version from project.godot"
    exit 1
}

Write-Host "Version: $Version"

# Read protocol version from project.godot.
if ($projectGodot -match 'config/protocol_version=(\d+)') {
    $ProtocolVersion = $Matches[1]
} else {
    Write-Error "Could not read config/protocol_version from project.godot"
    exit 1
}

Write-Host "Protocol version: $ProtocolVersion"
Write-Host ""

# Step 1: Sync GAME_VERSION and PROTOCOL_VERSION
# in template.yaml.
Write-Host "[1/3] Syncing versions in template.yaml..." -ForegroundColor Yellow

$templatePath = "backend/template.yaml"
$templateContent = Get-Content $templatePath -Raw

if ($templateContent -match 'GAME_VERSION:\s*"([^"]+)"') {
    $currentVersion = $Matches[1]
    if ($currentVersion -ne $Version) {
        $templateContent = $templateContent -replace (
            'GAME_VERSION:\s*"[^"]+"'),
            "GAME_VERSION: `"$Version`""
        Write-Host "  Updated GAME_VERSION: $currentVersion -> $Version" -ForegroundColor Green
    } else {
        Write-Host "  GAME_VERSION already $Version" -ForegroundColor DarkGray
    }
} else {
    Write-Error "Could not find GAME_VERSION in $templatePath"
    exit 1
}

if ($templateContent -match 'PROTOCOL_VERSION:\s*"(\d+)"') {
    $currentProtocol = $Matches[1]
    if ($currentProtocol -ne $ProtocolVersion) {
        $templateContent = $templateContent -replace (
            'PROTOCOL_VERSION:\s*"\d+"'),
            "PROTOCOL_VERSION: `"$ProtocolVersion`""
        Write-Host "  Updated PROTOCOL_VERSION: $currentProtocol -> $ProtocolVersion" -ForegroundColor Green
    } else {
        Write-Host "  PROTOCOL_VERSION already $ProtocolVersion" -ForegroundColor DarkGray
    }
} else {
    Write-Error "Could not find PROTOCOL_VERSION in $templatePath"
    exit 1
}

Set-Content -Path $templatePath -Value $templateContent -NoNewline

# Step 2: Discover current GameLift container fleet ID.
# Passed to the SAM deploy as a parameter override so the
# fleet warmup and idle-check Lambdas know which fleet to
# operate on. The fleet ID changes whenever the fleet is
# recreated (e.g., to switch billing type), so we look it
# up fresh on each deploy rather than hard-coding.
#
# Note: list-fleets returns managed EC2 fleets only, so
# container fleets require the separate list-container-fleets
# API.
Write-Host "[2/4] Looking up fleet ID..." -ForegroundColor Yellow

$FleetJson = aws gamelift list-container-fleets --profile $Profile --region $Region --output json
if ($LASTEXITCODE -ne 0) {
    Write-Error "aws gamelift list-container-fleets failed"
    exit 1
}

$FleetData = $FleetJson | ConvertFrom-Json
$Fleets = $FleetData.ContainerFleets
if ($null -eq $Fleets -or $Fleets.Count -eq 0) {
    Write-Warning "No GameLift container fleets found. Deploying with empty FleetId (warmup will be disabled)."
    $FleetId = ""
} elseif ($Fleets.Count -gt 1) {
    $FleetId = $Fleets[0].FleetId
    Write-Warning "Multiple container fleets found. Using the first: $FleetId"
} else {
    $FleetId = $Fleets[0].FleetId
    Write-Host "  Fleet ID: $FleetId" -ForegroundColor Green
}

# Step 3: SAM build.
Write-Host "[3/4] Running sam build..." -ForegroundColor Yellow

Push-Location backend
try {
    # --use-container builds inside a Lambda-like Docker
    # image so native extensions (bcrypt) are compiled
    # for Amazon Linux, not the local OS.
    sam build --use-container --profile $Profile --region $Region
    if ($LASTEXITCODE -ne 0) {
        Write-Error "sam build failed"
        exit 1
    }
    Write-Host "Build complete." -ForegroundColor Green

    # Step 4: SAM deploy. Passes FleetId as a parameter
    # override. AlertEmail is set via samconfig.toml (one-time
    # configuration); it is intentionally not passed here.
    Write-Host "[4/4] Running sam deploy..." -ForegroundColor Yellow

    $deployOutput = sam deploy --no-confirm-changeset --profile $Profile --region $Region `
        --parameter-overrides "FleetId=$FleetId" 2>&1 | Tee-Object -Variable stdoutCopy
    $deployExit = $LASTEXITCODE
    # sam deploy exits non-zero when there are no changes to
    # deploy. That is a success outcome for our purposes, so
    # match the specific message and treat it as a no-op.
    if ($deployExit -ne 0) {
        $joinedOutput = ($deployOutput | Out-String)
        if ($joinedOutput -match "No changes to deploy") {
            Write-Host "No changes to deploy (stack up to date)." -ForegroundColor DarkGray
        } else {
            Write-Error "sam deploy failed"
            exit 1
        }
    }
    Write-Host "Deploy complete." -ForegroundColor Green
} finally {
    Pop-Location
}

Write-Host ""
Write-Host "=== Backend deployment complete ===" -ForegroundColor Green
Write-Host "Version: $Version"
