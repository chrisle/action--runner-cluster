# Windows runner container entrypoint.
#
# arc pre-registers the runner and passes its just-in-time configuration in
# ARC_JITCONFIG. The runner takes one job and exits; arc removes the container
# afterwards, so nothing the job wrote survives into the next one.

$ErrorActionPreference = 'Stop'

if (-not $env:ARC_JITCONFIG) {
    Write-Error @"
ARC_JITCONFIG is not set.
This image is meant to be started by arc, which supplies the runner's
just-in-time configuration. Running it by hand will not work.
"@
    exit 1
}

Set-Location C:\actions-runner

$pool = if ($env:ARC_POOL) { $env:ARC_POOL } else { 'unknown' }
$name = if ($env:ARC_RUNNER_NAME) { $env:ARC_RUNNER_NAME } else { 'unknown' }
Write-Host "arc: starting runner $name in pool $pool"

# run.cmd is used rather than Runner.Listener directly so the runner's own
# environment setup and exit-code handling stay intact.
& C:\actions-runner\run.cmd --jitconfig $env:ARC_JITCONFIG
exit $LASTEXITCODE
