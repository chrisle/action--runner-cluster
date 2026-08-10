# Install arc as a Windows service.
#
# Only needed if you run the orchestrator itself on Windows. The more common
# layout is a single orchestrator on your Mac or a Linux box that reaches the
# Windows Docker daemon over ssh:// — one orchestrator, one GitHub poller, one
# place to look at status.
#
#   .\install-service.ps1 -ArcPath C:\arc\arc.exe -ConfigPath C:\arc\arc.yaml
#
# Uninstall:
#   sc.exe delete arc

[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$ArcPath,
    [Parameter(Mandatory = $true)][string]$ConfigPath,
    [string]$ServiceName = 'arc',
    [string]$GitHubOrg,
    [string]$GitHubToken
)

$ErrorActionPreference = 'Stop'

if (-not ([Security.Principal.WindowsPrincipal] [Security.Principal.WindowsIdentity]::GetCurrent()
      ).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw "Run this from an elevated PowerShell session."
}

foreach ($path in @($ArcPath, $ConfigPath)) {
    if (-not (Test-Path $path)) { throw "Not found: $path" }
}

if (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue) {
    Write-Host "Stopping and removing the existing $ServiceName service..."
    Stop-Service -Name $ServiceName -Force -ErrorAction SilentlyContinue
    sc.exe delete $ServiceName | Out-Null
    Start-Sleep -Seconds 2
}

# The service account needs to reach the Docker daemon. LocalSystem works with
# Docker Desktop's default named pipe permissions; a dedicated account needs to
# be added to the docker-users group.
$binPath = "`"$ArcPath`" run -config `"$ConfigPath`""

Write-Host "Creating service $ServiceName"
New-Service -Name $ServiceName `
            -BinaryPathName $binPath `
            -DisplayName 'arc — GitHub Actions runner orchestrator' `
            -Description 'Scales ephemeral self-hosted GitHub Actions runners.' `
            -StartupType Automatic | Out-Null

# Credentials go in the machine environment so they are not visible in the
# service's command line, which any user can read.
if ($GitHubOrg) {
    [Environment]::SetEnvironmentVariable('GITHUB_ORG', $GitHubOrg, 'Machine')
}
if ($GitHubToken) {
    [Environment]::SetEnvironmentVariable('GITHUB_TOKEN', $GitHubToken, 'Machine')
}

# Restart on failure rather than leaving the cluster unattended, but back off so
# a bad config does not hammer the GitHub API.
sc.exe failure $ServiceName reset= 300 actions= restart/30000/restart/30000/restart/60000 | Out-Null

Write-Host "Starting $ServiceName"
Start-Service -Name $ServiceName
Get-Service -Name $ServiceName

Write-Host ""
Write-Host "Check it with:  & '$ArcPath' doctor -config '$ConfigPath'"
Write-Host "Service logs:   Get-EventLog -LogName Application -Source $ServiceName -Newest 50"
