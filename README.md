# arc — GitHub Actions runner cluster orchestrator

One self-hosted runner cluster for every project in your org, across Linux,
Windows and macOS. Set a minimum and a maximum per pool; arc watches for queued
jobs, creates runners as work arrives, and destroys them completely when it
drains away.

Every runner is **ephemeral**: it takes exactly one job, unregisters itself, and
is deleted along with its entire filesystem. Nothing survives into the next job
— no caches, no checked-out repos, no toolchain downloads, no disk creeping
toward full.

```
$ arc status
org acme · 14 repos watched · updated 3s ago
github rate limit: 4821/5000 remaining, resets 41m12s

POOL     PROVIDER  MIN  MAX  LIVE  BUSY  IDLE  STARTING  QUEUED  DESIRED  STATE
linux    docker    1    8    5     4     1     0         3       7        scaling up for queued jobs
windows  docker    0    4    1     1     0     0         0       1        at target
macos    process   1    3    1     0     1     0         0       1        at target
```

## The one thing to know first

**Docker cannot run macOS runners.** Docker Desktop on a Mac runs Linux
containers inside a Linux VM — a "macOS container" would be a Linux runner that
cannot touch Xcode, code signing, or the iOS simulators. Apple does not permit
containerizing macOS, and no tool works around that.

So arc has two providers:

| Platform | Provider  | What a runner is                                                |
| -------- | --------- | --------------------------------------------------------------- |
| Linux    | `docker`  | A container, destroyed after one job                             |
| Windows  | `docker`  | A Windows container (needs a Windows host in Windows-container mode) |
| macOS    | `process` | A runner process in a private, copy-on-write directory, deleted after one job |

`arc doctor` rejects a macOS pool configured for Docker rather than letting you
find out from a runner that mysteriously has no Xcode.

## How the disposal guarantee works

Both providers give you the same promise by different means:

- **Docker** — the container is removed with its anonymous volumes (`v=true`),
  so the whole writable layer goes. Optionally `work_tmpfs: true` puts the job
  workspace in RAM, so it never reaches disk at all.
- **Process** — a pristine, never-configured runner installation is cloned per
  job using APFS copy-on-write (`cp -Rc`), which is near-instant and costs
  almost no disk despite the runner being a few hundred megabytes. When the job
  finishes the entire clone is `rm -rf`'d.

The process provider's one honest limitation: anything a job writes *outside*
its instance directory — a real `~/Library` cache, `/tmp`, Homebrew — is beyond
its control. That is the unavoidable difference between a process and a
container. `RUNNER_TEMP` and `TMPDIR` are pointed inside the instance directory
to cover the common cases.

## Scaling

Each tick, for every pool:

```
desired = clamp(max(min, busy + queued), min, max)
```

`busy + queued` rather than just `queued` because runners are ephemeral: a
runner executing a job cannot also serve a queued one, so in-flight work and
waiting work both need capacity. `min` keeps warm runners so the first job of
the day doesn't pay full startup cost.

Scale-down is conservative on purpose:

- Only **idle** runners are ever removed. A busy one is executing someone's
  build. GitHub itself refuses to deregister a busy runner (422), which is the
  backstop if the orchestrator's view is stale.
- A surplus runner must be continuously idle for `idle_timeout` before it goes,
  so a burst of jobs arriving seconds apart doesn't thrash create/destroy.
- Ephemeral runners exit on their own after one job anyway, so most scale-down
  is just arc reaping what already finished and topping back up to `min`.

### Which pool gets a job

A pool can serve a job when the pool's labels are a **superset** of the job's
`runs-on` labels — the same rule GitHub uses to route work. When several pools
qualify, the one with the fewest extra labels wins, so a plain
`runs-on: [self-hosted, linux]` job doesn't consume your scarce
`linux + gpu` machines. If the best-fit pool is at `max`, the job spills to
another eligible pool instead of waiting.

Jobs targeting GitHub-hosted images (`ubuntu-latest`, `macos-14`, …) are
ignored — GitHub runs those itself, and treating them as demand would spin up
local runners that never receive work.

## Setup

### 1. Build

```sh
make build            # ./bin/arc
make install          # into $GOPATH/bin
make release          # cross-compiled binaries in dist/
```

No runtime dependencies — it is a single static binary.

### 2. Credentials

Either a classic PAT with the **`admin:org`** scope:

```sh
export GITHUB_ORG=your-org
export GITHUB_TOKEN=ghp_...
```

…or, better for anything long-lived, a GitHub App with
**`organization_self_hosted_runners: write`**. Installation tokens are minted
hourly and scoped to the installation, and there's no personal account whose
departure breaks the cluster:

```yaml
github:
  org: your-org
  app:
    app_id: 123456
    installation_id: 7890123
    private_key_path: /etc/arc/app-key.pem
```

### 3. Runner images and templates

```sh
# Linux (build for the arch you run on, or multi-arch)
make image-linux REGISTRY=ghcr.io/your-org
make image-linux-multiarch REGISTRY=ghcr.io/your-org

# Windows — must run ON a Windows host with Docker Desktop in Windows-container mode
make image-windows REGISTRY=ghcr.io/your-org

# macOS — run once on the Mac that will host runners
make macos-template
```

The images contain an **unconfigured** runner. arc pre-registers each runner
with GitHub and passes a just-in-time config at start, so one image serves every
pool and every project, and nothing in it is tied to an org.

### 4. Configure

```sh
cp config.example.yaml arc.yaml
$EDITOR arc.yaml
arc doctor
```

`arc doctor` checks the config, the credentials and their scopes, repo
visibility, every Docker daemon, and every runner template — and reports all
failures at once rather than stopping at the first.

### 5. Run

```sh
arc run
```

As a service: `deploy/systemd/arc.service` (Linux),
`deploy/launchd/com.arc.orchestrator.plist` (macOS — must be a **LaunchAgent**,
not a LaunchDaemon, or code signing will fail),
`deploy/windows/install-service.ps1` (Windows).

## Host tooling for process pools

Docker pools get their tools from the image. **Process pools get them from the
host**: each ephemeral runner is a clean clone of the runner template, but it
executes directly on the machine, so every CLI a workflow step calls must
already be installed there. `setup-*` actions that download their own toolchain
(`actions/setup-node`, `swatinem/rust-cache`) still work; anything a workflow
assumes was preinstalled on a GitHub-hosted image does not.

What the current fleet needed to take over a Tauri desktop-app release
workflow (build + sign + notarize on macOS, build + Azure Trusted Signing on
Windows, publish via `gh` on Linux) — a reasonable checklist for similar
workloads:

| Pool | Host-provided tools |
| --- | --- |
| linux | `gh`, `jq`, plus the runner system libs (`installdependencies.sh`) |
| macos | Xcode Command Line Tools, `rustup` |
| windows | VS Build Tools 2022 (MSVC), Windows SDK (`signtool`), `pwsh` 7, .NET 8 runtime, `rustup` |

Three gotchas:

- **PATH is captured when arc starts.** Runners inherit arc's environment, so a
  tool installed after arc came up (e.g. `rustup-init` appending to the Windows
  user PATH) is invisible to jobs until arc restarts. Restart when
  `/v1/status` shows `busy: 0`.
- **On Windows, `bash` must be Git Bash — not WSL.** Actions runs every
  `shell: bash` step, and the bash half of many composite actions
  (`dtolnay/rust-toolchain` among them), through whatever `bash` PATH resolves
  to first. If that is WSL's app-execution alias under `WindowsApps`, every one
  of those steps fails with a mangled path:

  ```
  /bin/bash: C:Userschris.arcinstances..._temp_xyz.sh: No such file or directory
  ```

  WSL bash cannot open a Windows path, and the backslashes vanish as escapes.
  GitHub-hosted runners ship Git Bash first on PATH; a workstation usually does
  not. Fix it for runners alone by prepending `C:\Program Files\Git\bin` in
  arc's launcher script rather than to the user PATH, so interactive shells
  keep whatever `bash` their owner expects.
- Keep host tools out of the runner template itself — templates must stay
  unconfigured and generic.

## Topology

Run **one** orchestrator. It polls GitHub once and drives every pool:

```
                      ┌──────────────────────────────┐
                      │  arc  (one process)          │
   GitHub API ◀──────▶│  polls queued jobs           │
                      │  mints JIT runner configs    │
                      └───┬───────────┬───────────┬──┘
        unix:// socket    │           │           │   ssh://
                          ▼           ▼           ▼
                   Linux docker   macOS process   Windows docker
                   (containers)   (cloned dirs)   (Windows containers)
```

Remote daemons are reached over `ssh://user@host`, which uses your existing SSH
keys and exposes no daemon port. `tcp://` with client certificates works too;
unauthenticated `tcp://` does not, deliberately.

Two orchestrators against one org would both poll and both scale, fighting over
the same demand signal. Don't.

## Adjusting min and max

Live, without a restart, persisted across restarts:

```sh
arc scale linux --max 16        # raise the ceiling for a big migration
arc scale linux --min 3         # keep more warm
arc scale linux --reset         # back to the config file's values

arc drain windows               # stop creating; running jobs finish untouched
arc resume windows
```

## Commands

| Command | What it does |
| --- | --- |
| `arc run` | Start the orchestrator |
| `arc status [-wide] [-json] [-watch 2s]` | Pools, runners, queued jobs |
| `arc scale <pool> [-min N] [-max N] [-reset]` | Change limits at runtime |
| `arc drain <pool>` / `arc resume <pool>` | Stop / restart runner creation |
| `arc logs <pool> <instance-id>` | Tail a runner's output |
| `arc doctor` | Check everything before it bites you |

`GET /metrics` on the control API serves Prometheus gauges for live, busy, idle,
desired, queued and rate-limit remaining.

## API cost

There is no org-wide "list queued jobs" endpoint, so arc walks each repo's
active workflow runs. Every request is conditional on an ETag, and GitHub does
not charge a `304` against the rate limit — so quiet repos are free to poll.

If your org is large, narrow the work with `github.repos`:

```yaml
github:
  poll_interval: 15s
  repos:
    active_within: 720h        # skip repos with no push in 30 days
    include: ["service-*"]
    exclude: ["archived-*"]
```

`arc doctor` warns when your poll interval and repo count could outrun the
limit.

## Security notes

- The control API binds to loopback and changes scaling and reads runner logs.
  Set `server.token` if you must expose it, and put TLS in front of it.
- `docker_in_docker: true` gives every workflow in the org root-equivalent
  control of that host's Docker daemon. It is off by default for that reason.
- Self-hosted runners and public repositories are a bad combination in general:
  anyone who can open a pull request can run code on your machine. The
  ephemeral-container model limits the blast radius but does not eliminate it.
  GitHub's own guidance is to use self-hosted runners with private repos only.

## What's deliberately not here

- **Webhooks.** Polling with conditional requests costs little and needs no
  public endpoint, no tunnel and no inbound firewall rule.
- **Config hot-reload.** Limits change live with `arc scale`; anything else is a
  restart, which is cheap because runners are ephemeral and are left to finish.
- **Tart / VM runners.** The provider interface has room for it. macOS VMs would
  give per-job isolation the process provider can't, at a real cost in startup
  time and disk.

## Development

```sh
make check          # fmt, vet, test
make test-unit      # skip the tests that need a Docker daemon
make crosscheck     # verify all five target platforms still compile
```

The Docker integration tests run against your local daemon, create real
containers under a test-only pool label, and clean up after themselves. They
skip automatically when no daemon is reachable.
