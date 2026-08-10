#!/usr/bin/env bash
#
# Runner container entrypoint.
#
# arc pre-registers the runner with GitHub and passes the resulting just-in-time
# configuration in ARC_JITCONFIG. The runner takes exactly one job, deregisters
# itself, and exits; arc then removes the container, taking the workspace and
# every cache with it.

set -euo pipefail

if [[ -z "${ARC_JITCONFIG:-}" ]]; then
  echo "ARC_JITCONFIG is not set." >&2
  echo "This image is meant to be started by arc, which supplies the runner's" >&2
  echo "just-in-time configuration. Running it by hand will not work." >&2
  exit 1
fi

cd /home/runner

# Forward termination to the runner so it gets the chance to report the job as
# cancelled and unregister, instead of leaving GitHub waiting on a dead runner.
runner_pid=""
forward() {
  if [[ -n "${runner_pid}" ]]; then
    kill -TERM "${runner_pid}" 2>/dev/null || true
    wait "${runner_pid}" 2>/dev/null || true
  fi
}
trap forward TERM INT

echo "arc: starting runner ${ARC_RUNNER_NAME:-unknown} in pool ${ARC_POOL:-unknown}"

./run.sh --jitconfig "${ARC_JITCONFIG}" &
runner_pid=$!
wait "${runner_pid}"
