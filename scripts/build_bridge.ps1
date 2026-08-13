# Build network-ultra-bridge.exe with size-optimised flags + optional UPX pack.
#
# Why we do this:
#   * Default Go build → 9.8 MB (debug symbols + DWARF + full Go runtime)
#   * `-ldflags="-s -w" -trimpath` strips them → 6.9 MB (-30%)
#   * UPX --best --lzma packs it → 2.2 MB (-78% from default)
#
# UPX is never downloaded. If an operator has explicitly installed it on PATH,
# the script may use it; set $env:NU_NO_UPX=1 for the unpacked release artifact
# (also avoids antivirus false positives sometimes caused by packed binaries).
#
# Usage:
#   pwsh scripts\build_bridge.ps1
#   $env:NU_NO_UPX = "1"; pwsh scripts\build_bridge.ps1   # skip UPX
#
param([string]$Version = '')

$ErrorActionPreference = 'Stop'

$ScriptDir   = Split-Path -Parent $MyInvocation.MyCommand.Path
$ServerRoot  = Split-Path -Parent $ScriptDir
$Output      = Join-Path $ServerRoot 'bin\network-ultra-bridge.exe'
$ClientVersionFile = Join-Path (Split-Path -Parent $ServerRoot) 'client\version.txt'
$GoCommand = (Get-Command go -CommandType Application -ErrorAction Stop).Source

if ([string]::IsNullOrWhiteSpace($Version) -and (Test-Path -LiteralPath $ClientVersionFile)) {
    $Version = (Get-Content -LiteralPath $ClientVersionFile -Raw).Trim()
}
if ($Version -notmatch '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$') {
    throw "A release semver is required. Pass -Version x.y.z or provide $ClientVersionFile; dev is forbidden."
}

$PreviousGOOS = [Environment]::GetEnvironmentVariable('GOOS', 'Process')
$PreviousGOARCH = [Environment]::GetEnvironmentVariable('GOARCH', 'Process')
$PreviousCGO = [Environment]::GetEnvironmentVariable('CGO_ENABLED', 'Process')
Push-Location $ServerRoot
try {
    # Do not inherit a developer shell's cross-build settings. This script has
    # one artifact contract: a pure-Go Windows amd64 bridge executable.
    $env:GOOS = 'windows'
    $env:GOARCH = 'amd64'
    $env:CGO_ENABLED = '0'

# 1. go build with size-optimised flags
Write-Host "[1/3] go build with -ldflags='-s -w' -trimpath..." -ForegroundColor Cyan
New-Item -ItemType Directory -Path (Split-Path -Parent $Output) -Force | Out-Null
$BuildId = "network-ultra-bridge/$Version/windows-amd64"
& $GoCommand build -trimpath -ldflags="-s -w -buildid=$BuildId -X main.bridgeVersion=$Version" -o $Output ./cmd/bridge
if ($LASTEXITCODE -ne 0) { throw "go build failed" }
$ReportedBuildId = (& $GoCommand tool buildid $Output).Trim()
if ($LASTEXITCODE -ne 0 -or $ReportedBuildId -ne $BuildId) {
    throw "bridge build ID verification failed: expected '$BuildId', got '$ReportedBuildId'"
}
$BuildInfo = (& $GoCommand version -m $Output | Out-String)
if ($LASTEXITCODE -ne 0 `
    -or $BuildInfo -notmatch '(?m)^\s*build\s+GOOS=windows\s*$' `
    -or $BuildInfo -notmatch '(?m)^\s*build\s+GOARCH=amd64\s*$' `
    -or $BuildInfo -notmatch '(?m)^\s*build\s+CGO_ENABLED=0\s*$') {
    throw "bridge target metadata verification failed; expected windows/amd64 with CGO disabled"
}
$ReportedVersion = (& $Output --version).Trim()
if ($LASTEXITCODE -ne 0 -or $ReportedVersion -ne $Version) {
    throw "bridge version verification failed: expected '$Version', got '$ReportedVersion'"
}
$origSize = (Get-Item $Output).Length
Write-Host ("    {0,7:N2} MB stripped" -f ($origSize / 1MB)) -ForegroundColor Green

if ($env:NU_NO_UPX -eq "1") {
    Write-Host "[2/3] UPX skipped (NU_NO_UPX=1)" -ForegroundColor Yellow
    Write-Host "[3/3] done. final = $($origSize / 1MB) MB" -ForegroundColor Green
    return
}

# 2. Locate an explicitly installed UPX. Release builds never download an
# unverified executable from a mutable mirror.
$upx = $null
$onPath = Get-Command upx -CommandType Application -ErrorAction SilentlyContinue
if ($onPath) {
    $upx = $onPath.Path
    Write-Host "[2/3] UPX on PATH: $upx" -ForegroundColor Cyan
} else {
    Write-Host "[2/3] UPX not installed; keeping the verified unpacked binary" -ForegroundColor Yellow
    Write-Host "[3/3] done. final = $($origSize / 1MB) MB, version = $Version" -ForegroundColor Green
    return
}

# 3. Pack with UPX (--best --lzma is the most aggressive preset)
Write-Host "[3/3] UPX --best --lzma..." -ForegroundColor Cyan
& $upx --best --lzma --quiet $Output
if ($LASTEXITCODE -ne 0) { throw "upx pack failed" }
$finalSize = (Get-Item $Output).Length
$savings = 1.0 - ($finalSize / $origSize)
$PackedVersion = (& $Output --version).Trim()
if ($LASTEXITCODE -ne 0 -or $PackedVersion -ne $Version) {
    throw "packed bridge version verification failed: expected '$Version', got '$PackedVersion'"
}
Write-Host ("    {0,7:N2} MB packed ({1:P0} smaller than stripped)" -f ($finalSize / 1MB), $savings) -ForegroundColor Green
Write-Host ""
Write-Host "✓ done. $Output" -ForegroundColor Green
} finally {
    Pop-Location
    if ($null -eq $PreviousGOOS) { Remove-Item Env:\GOOS -ErrorAction SilentlyContinue } else { $env:GOOS = $PreviousGOOS }
    if ($null -eq $PreviousGOARCH) { Remove-Item Env:\GOARCH -ErrorAction SilentlyContinue } else { $env:GOARCH = $PreviousGOARCH }
    if ($null -eq $PreviousCGO) { Remove-Item Env:\CGO_ENABLED -ErrorAction SilentlyContinue } else { $env:CGO_ENABLED = $PreviousCGO }
}
