# escape=`
#
# Windows runner image.
#
# This only builds and runs on a Windows host with Docker Desktop switched to
# Windows containers (right-click the tray icon, "Switch to Windows containers").
# A Linux daemon cannot build or run it, and `arc doctor` will say so rather
# than letting you find out from a mysteriously Linux-shaped runner.
#
# The base image tag must match the host's Windows build closely enough for
# process isolation; ltsc2022 pairs with Windows Server 2022 and Windows 11.
#
#   docker build -t ghcr.io/YOU/arc-runner:windows images\windows

FROM mcr.microsoft.com/windows/servercore:ltsc2022

ARG RUNNER_VERSION=2.336.0

SHELL ["powershell", "-NoLogo", "-ExecutionPolicy", "Bypass", "-Command", "$ErrorActionPreference='Stop'; $ProgressPreference='SilentlyContinue';"]

WORKDIR C:\actions-runner

RUN Invoke-WebRequest `
      -Uri "https://github.com/actions/runner/releases/download/v$env:RUNNER_VERSION/actions-runner-win-x64-$env:RUNNER_VERSION.zip" `
      -OutFile runner.zip; `
    Expand-Archive -Path runner.zip -DestinationPath . ; `
    Remove-Item runner.zip

# git is not present in servercore and essentially every workflow needs it.
RUN Invoke-WebRequest `
      -Uri "https://github.com/git-for-windows/git/releases/download/v2.47.1.windows.1/MinGit-2.47.1-64-bit.zip" `
      -OutFile mingit.zip; `
    Expand-Archive -Path mingit.zip -DestinationPath C:\mingit; `
    Remove-Item mingit.zip; `
    $path = [Environment]::GetEnvironmentVariable('PATH', 'Machine'); `
    [Environment]::SetEnvironmentVariable('PATH', "$path;C:\mingit\cmd", 'Machine')

COPY entrypoint.ps1 C:/actions-runner/entrypoint.ps1

ENTRYPOINT ["powershell", "-NoLogo", "-ExecutionPolicy", "Bypass", "-File", "C:\\actions-runner\\entrypoint.ps1"]
