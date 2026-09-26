# Builds dist\gdrive-ignore.exe (a single self-contained exe).
#   .\scripts\build.ps1 [-Version 0.1.0]
param(
    [string]$Version = ""
)
$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    $portable = Join-Path $env:LOCALAPPDATA "Programs\go\bin"
    if (Test-Path (Join-Path $portable "go.exe")) { $env:Path = "$portable;$env:Path" }
    else { throw "Go is not installed (https://go.dev/dl/)" }
}

if (-not $Version) {
    $Version = (git describe --tags --always --dirty 2>$null)
    if (-not $Version) { $Version = "dev" }
}
# Windows version resources need a numeric x.y.z.w form.
$numeric = "0.0.0.0"
if ($Version -match '^v?(\d+)\.(\d+)\.(\d+)') { $numeric = "$($Matches[1]).$($Matches[2]).$($Matches[3]).0" }

Write-Host "Building gdrive-ignore $Version"
go run github.com/tc-hib/go-winres@v0.3.3 make `
    --in cmd/gdrive-ignore/winres/winres.json `
    --out cmd/gdrive-ignore/rsrc `
    --arch amd64 `
    --product-version $numeric --file-version $numeric
if ($LASTEXITCODE -ne 0) { throw "go-winres failed" }

New-Item -ItemType Directory -Force dist | Out-Null
$env:CGO_ENABLED = "0"
# Console launcher, embedded into the main exe and installed as gdrive-ignore.com.
go build -trimpath -ldflags "-s -w" -o internal/assets/cli/gdrive-ignore.com ./cmd/gdrive-ignore-cli
if ($LASTEXITCODE -ne 0) { throw "launcher build failed" }
Copy-Item internal/assets/cli/gdrive-ignore.com dist/gdrive-ignore.com
go build -trimpath -ldflags "-s -w -H windowsgui -X main.version=$Version" -o dist/gdrive-ignore.exe ./cmd/gdrive-ignore
if ($LASTEXITCODE -ne 0) { throw "go build failed" }

$size = (Get-Item dist/gdrive-ignore.exe).Length / 1MB
Write-Host ("dist\gdrive-ignore.exe  {0:N1} MB" -f $size)
