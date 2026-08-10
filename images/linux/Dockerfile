# Linux runner image.
#
# The runner is installed but never configured: arc passes a just-in-time
# configuration at container start, so nothing about this image is tied to a
# particular org, repo or runner identity. That is what lets one image serve
# every pool and every project.
#
# Build for the architecture you will run on:
#   docker build -t ghcr.io/YOU/arc-runner:linux images/linux
#   docker buildx build --platform linux/amd64,linux/arm64 -t ... --push images/linux

FROM ubuntu:24.04

ARG RUNNER_VERSION=2.336.0
ARG TARGETARCH=amd64
ARG DEBIAN_FRONTEND=noninteractive

# Packages the runner itself needs, plus the handful every workflow assumes
# exists. Keep this list short: anything heavier belongs in a derived image so
# the base stays quick to pull on a cold host.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      git \
      gnupg \
      jq \
      libicu74 \
      lsb-release \
      openssh-client \
      sudo \
      tar \
      unzip \
      zip \
 && rm -rf /var/lib/apt/lists/*

# The runner refuses to execute as root unless RUNNER_ALLOW_RUNASROOT is set,
# and running jobs as root makes every file a job creates awkward to clean up
# from the host. A dedicated user avoids both problems.
RUN useradd -m -s /bin/bash runner \
 && echo "runner ALL=(ALL) NOPASSWD: ALL" > /etc/sudoers.d/runner \
 && chmod 0440 /etc/sudoers.d/runner

USER runner
WORKDIR /home/runner

RUN case "${TARGETARCH}" in \
      amd64) RUNNER_ARCH=x64 ;; \
      arm64) RUNNER_ARCH=arm64 ;; \
      *) echo "unsupported architecture: ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
 && curl -fsSL -o runner.tar.gz \
      "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-${RUNNER_ARCH}-${RUNNER_VERSION}.tar.gz" \
 && tar xzf runner.tar.gz \
 && rm runner.tar.gz

USER root
RUN /home/runner/bin/installdependencies.sh
USER runner

COPY --chown=runner:runner entrypoint.sh /home/runner/entrypoint.sh
RUN chmod +x /home/runner/entrypoint.sh

ENTRYPOINT ["/home/runner/entrypoint.sh"]
