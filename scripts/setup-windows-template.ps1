<#
.SYNOPSIS
    Prepare the Windows runner template that the process provider clones per job.

.DESCRIPTION
    Run this once on the Windows machine that will host runners. It downloads an
    actions-runner release and leaves it *unconfigured* — arc supplies a
    just-in-time configuration at launch, so the template must never have had
    config.cmd run against it.

    Use this when the machine cannot run Windows containers. Windows 11 Home is
    the common case: it has no Hyper-V, and Docker Desktop cannot run Windows
    containers without it. On Windows Pro, Enterprise, Server or any edition
    that does support them, prefer the docker provider — see images/windows.

.PARAMETER TargetDir
    Where to install the template. Default: $HOME\.arc\runner-template

.PARAMETER Version
    Runner version to install, e.g. 2.336.0. Default: the latest release.

.EXAMPLE
    .\scripts\setup-windows-template.ps1

.EXAMPLE
    .\scripts\setup-windows-template.ps1 -Version 2.336.0
#>
[CmdletBinding()]
param(
    [string]$TargetDir = (Join-Path $HOME '.arc\runner-template'),
    [string]$Version
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

if (-not $IsWindows -and $PSVersionTable.PSVersion.Major -ge 6) {
    Write-Error "This script sets up a Windows runner template but is not running on Windows."
    exit 1
}

# The runner ships x64 and arm64 builds. Match the OS, not the PowerShell
# process, which can be 32-bit on a 64-bit machine.
$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { 'x64' }
    'ARM64' { 'arm64' }
    'x86'   { if ([Environment]::Is64BitOperatingSystem) { 'x64' } else { 'x86' } }
    default { $env:PROCESSOR_ARCHITECTURE }
}
if ($arch -eq 'x86') {
    Write-Error "32-bit Windows is not supported by the Actions runner."
    exit 1
}

if (-not $Version) {
    Write-Host "Resolving the latest runner release..."
    try {
        $rel = Invoke-RestMethod -Uri 'https://api.github.com/repos/actions/runner/releases/latest' `
            -Headers @{ 'User-Agent' = 'arc-setup' }
        $Version = $rel.tag_name -replace '^v', ''
    } catch {
        Write-Error @"
Could not resolve the latest version: $_
Pass one explicitly:
  .\scripts\setup-windows-template.ps1 -Version 2.336.0
"@
        exit 1
    }
}

Write-Host "Installing actions-runner $Version (win-$arch) into $TargetDir"

# A template that has been configured carries .runner and .credentials, and
# cloning those into every instance makes runners fight over one identity.
# Starting from an empty directory is what guarantees that cannot happen.
if (Test-Path $TargetDir) {
    $existing = Get-ChildItem -Path $TargetDir -Force -ErrorAction SilentlyContinue
    if ($existing) {
        Write-Host "$TargetDir already exists and is not empty."
        $reply = Read-Host "Delete it and reinstall? [y/N]"
        if ($reply -notmatch '^[Yy]') {
            Write-Host "Left alone. Nothing to do."
            exit 0
        }
        Remove-Item -Path $TargetDir -Recurse -Force
    }
}
New-Item -ItemType Directory -Path $TargetDir -Force | Out-Null

$zipName = "actions-runner-win-$arch-$Version.zip"
$url = "https://github.com/actions/runner/releases/download/v$Version/$zipName"
$tmp = Join-Path ([System.IO.Path]::GetTempPath()) $zipName

Write-Host "Downloading $url"
try {
    # Invoke-WebRequest's progress bar makes large downloads dramatically
    # slower in Windows PowerShell; suppressing it is worth several minutes.
    $prev = $ProgressPreference
    $ProgressPreference = 'SilentlyContinue'
    Invoke-WebRequest -Uri $url -OutFile $tmp -UseBasicParsing
} catch {
    Write-Error @"
Download failed: $_

Check that version $Version exists:
  https://github.com/actions/runner/releases
"@
    exit 1
} finally {
    $ProgressPreference = $prev
}

Write-Host "Extracting..."
try {
    Expand-Archive -Path $tmp -DestinationPath $TargetDir -Force
} finally {
    Remove-Item -Path $tmp -Force -ErrorAction SilentlyContinue
}

# Files downloaded from the internet carry a Zone.Identifier stream that makes
# Windows block execution. The runner would fail to start with no clear reason.
Write-Host "Clearing the downloaded-from-internet mark..."
Get-ChildItem -Path $TargetDir -Recurse -Force -File |
    Unblock-File -ErrorAction SilentlyContinue

$entry = Join-Path $TargetDir 'run.cmd'
if (-not (Test-Path $entry)) {
    Write-Error "Extraction did not produce run.cmd in $TargetDir; the archive may be corrupt."
    exit 1
}

# robocopy is what arc uses to clone this template per job. It ships with
# Windows, but confirm rather than let the first scale-up be the discovery.
if (-not (Get-Command robocopy -ErrorAction SilentlyContinue)) {
    Write-Error "robocopy was not found on PATH. arc needs it to clone this template."
    exit 1
}

$sizeMB = [math]::Round(
    ((Get-ChildItem -Path $TargetDir -Recurse -Force -File |
        Measure-Object -Property Length -Sum).Sum / 1MB), 0)

$fsType = try {
    $drive = (Get-Item $TargetDir).PSDrive.Name
    (Get-Volume -DriveLetter $drive -ErrorAction Stop).FileSystemType
} catch { 'unknown' }

Write-Host ""
Write-Host "Template ready: $TargetDir  ($sizeMB MB)"
Write-Host ""

# NTFS has no copy-on-write, so every job pays a full copy of the template.
# That is the single biggest surprise in running this provider on Windows, and
# it is worth stating in terms of what it costs at their configured max.
if ($fsType -eq 'ReFS') {
    Write-Host "Volume is ReFS, which supports block cloning — per-job copies should be cheap."
} else {
    Write-Host "Volume is $fsType. Windows has no copy-on-write on this filesystem, so each"
    Write-Host "runner costs a real $sizeMB MB copy. With max: 4 that is about $($sizeMB * 4) MB"
    Write-Host "of disk in flight, and a few seconds added to every job's start."
}

Write-Host ""
Write-Host "Add this pool to your arc.yaml:"
Write-Host ""
Write-Host "  - name: windows"
Write-Host "    labels: [self-hosted, windows, x64]"
Write-Host "    provider: process"
Write-Host "    min: 0"
Write-Host "    max: 2"
Write-Host "    idle_timeout: 10m"
Write-Host "    process:"
Write-Host "      template_dir: $TargetDir"
Write-Host ""
Write-Host "Then verify with:  arc doctor"
