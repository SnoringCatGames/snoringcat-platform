# Snoring Cat Platform Backend deploy script.
#
# Usage (from the repo root):
#   .\backend\scripts\deploy.ps1
#
# Optional flags:
#   -StackName     Override the CloudFormation stack name.
#                  Default: snoringcat-platform-backend
#   -Profile       AWS CLI profile. Default: hopnbop
#   -Region        AWS region. Default: us-west-2
#   -DefaultGameId The fallback game_id baked into the stack
#                  for legacy unauthenticated handlers.
#                  Default: hopnbop
#   -DryRun        Skip `sam deploy`; just build and report.
#
# What this script does NOT do:
#   - Touch the existing `hopnbop-backend` SAM stack or any
#     `hopnbop-*` DynamoDB tables. Those keep running until the
#     Phase 2 client cutover.
#   - Discover or bind to a GameLift fleet. Per-game fleet IDs
#     live in the `games` config table; this script doesn't
#     pass a fleet override at all.
#   - Run the migration script. See migrate-from-hopnbop.py
#     when you are ready for that step.
#
# Prerequisites:
#   - AWS SSO logged in: aws sso login --profile <Profile>
#   - Docker Desktop running (sam build --use-container)
#   - SAM CLI installed and on PATH

param(
    [string]$StackName = "snoringcat-platform-backend",
    [string]$Profile = "hopnbop",
    [string]$Region = "us-west-2",
    [string]$DefaultGameId = "hopnbop",
    [switch]$DryRun
)

$ErrorActionPreference = "Stop"

Write-Host "=== Snoring Cat Platform Backend deploy ===" -ForegroundColor Cyan
Write-Host "Stack:        $StackName"
Write-Host "Profile:      $Profile"
Write-Host "Region:       $Region"
Write-Host "DefaultGameId: $DefaultGameId"
Write-Host ""

# Resolve repo paths regardless of where the script was invoked
# from (works whether you ran it from repo root or backend/).
$ScriptDir  = Split-Path -Parent $MyInvocation.MyCommand.Definition
$BackendDir = Split-Path -Parent $ScriptDir

# --- Step 1: Read platform version from CHANGELOG.md.
# Source of truth is the [Unreleased] / latest [x.y.z] header
# at the top of CHANGELOG.md. We only use this for log output
# and CloudFormation tags; the per-game `protocol_version` is
# now in the games config table, not here.
Write-Host "[1/3] Reading platform version..." -ForegroundColor Yellow
$ChangelogPath = Join-Path (Split-Path -Parent $BackendDir) "CHANGELOG.md"
if (Test-Path $ChangelogPath) {
    $Changelog = Get-Content $ChangelogPath -Raw
    if ($Changelog -match '##\s*\[(\d+\.\d+\.\d+)\]') {
        $PlatformVersion = $Matches[1]
        Write-Host "  Platform version: $PlatformVersion" -ForegroundColor Green
    } else {
        $PlatformVersion = "unreleased"
        Write-Host "  No semver header in CHANGELOG.md; using 'unreleased'." -ForegroundColor DarkGray
    }
} else {
    $PlatformVersion = "unreleased"
    Write-Host "  CHANGELOG.md not found; using 'unreleased'." -ForegroundColor DarkGray
}
Write-Host ""

# --- Step 2: SAM build.
Write-Host "[2/3] Running sam build..." -ForegroundColor Yellow
Push-Location $BackendDir
try {
    # --use-container builds inside the AWS Lambda Python 3.12
    # image so native deps (bcrypt) are compiled for Amazon Linux,
    # not the local OS. Docker Desktop must be running.
    sam build --use-container --profile $Profile --region $Region
    if ($LASTEXITCODE -ne 0) {
        Write-Error "sam build failed"
        exit 1
    }
    Write-Host "Build complete." -ForegroundColor Green
    Write-Host ""

    # --- Step 3: SAM deploy.
    if ($DryRun) {
        Write-Host "[3/3] Skipping sam deploy (-DryRun)." -ForegroundColor DarkGray
        return
    }

    Write-Host "[3/3] Running sam deploy..." -ForegroundColor Yellow
    Write-Host "      (5-10 minutes; CloudFormation changeset phase is silent)" -ForegroundColor DarkGray

    # --no-confirm-changeset prevents interactive prompts (which
    # would hang non-interactive runs).
    $deployOutput = sam deploy `
        --stack-name $StackName `
        --no-confirm-changeset `
        --profile $Profile `
        --region $Region `
        --parameter-overrides "DefaultGameId=$DefaultGameId" `
        --tags "Project=snoringcat-platform" "Component=backend" "Version=$PlatformVersion" `
        2>&1 | Tee-Object -Variable stdoutCopy
    $deployExit = $LASTEXITCODE

    # sam deploy exits non-zero when there are no changes; treat
    # that as a no-op success.
    if ($deployExit -ne 0) {
        $joined = ($deployOutput | Out-String)
        if ($joined -match "No changes to deploy") {
            Write-Host "No changes to deploy (stack already up to date)." -ForegroundColor DarkGray
        } else {
            Write-Error "sam deploy failed (exit $deployExit)"
            exit 1
        }
    }
    Write-Host "Deploy complete." -ForegroundColor Green
} finally {
    Pop-Location
}

Write-Host ""
Write-Host "=== Platform backend deploy complete ===" -ForegroundColor Green
Write-Host "Version: $PlatformVersion"
Write-Host ""
Write-Host "Next steps:" -ForegroundColor Cyan
Write-Host "  - Populate the games config table for each game (see"
Write-Host "    docs/per-game-config.md for the schema)."
Write-Host "  - For Hop 'n Bop migration: run migrate-from-hopnbop.py"
Write-Host "    in dry-run mode first."
