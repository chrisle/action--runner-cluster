#!/usr/bin/env bash
#
# Prepare the macOS runner template that the process provider clones per job.
#
# Run this once on the Mac that will host runners. It downloads an
# actions-runner release and leaves it *unconfigured* — arc supplies a
# just-in-time configuration at launch, so the template must never have had
# config.sh run against it.
#
# Usage:
#   scripts/setup-macos-template.sh [target-dir] [runner-version]
#
# Default target: ~/.arc/runner-template

set -euo pipefail

TARGET="${1:-$HOME/.arc/runner-template}"
VERSION="${2:-}"

if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "This script sets up a macOS runner template but is running on $(uname -s)." >&2
  echo "Linux and Windows pools use container images instead; see images/." >&2
  exit 1
fi

case "$(uname -m)" in
  arm64) ARCH=arm64 ;;
  x86_64) ARCH=x64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

if [[ -z "${VERSION}" ]]; then
  echo "Resolving the latest runner release..."
  VERSION="$(curl -fsSL https://api.github.com/repos/actions/runner/releases/latest \
    | sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p' | head -1)"
  if [[ -z "${VERSION}" ]]; then
    echo "Could not resolve the latest version. Pass one explicitly:" >&2
    echo "  $0 ${TARGET} 2.336.0" >&2
    exit 1
  fi
fi

echo "Installing actions-runner ${VERSION} (osx-${ARCH}) into ${TARGET}"

if [[ -e "${TARGET}" ]]; then
  echo
  echo "${TARGET} already exists."
  read -r -p "Replace it? [y/N] " reply
  if [[ ! "${reply}" =~ ^[Yy]$ ]]; then
    echo "Leaving it alone."
    exit 0
  fi
  rm -rf "${TARGET}"
fi

mkdir -p "${TARGET}"
TARBALL="$(mktemp -t arc-runner).tar.gz"
trap 'rm -f "${TARBALL}"' EXIT

curl -fSL --progress-bar \
  -o "${TARBALL}" \
  "https://github.com/actions/runner/releases/download/v${VERSION}/actions-runner-osx-${ARCH}-${VERSION}.tar.gz"

tar xzf "${TARBALL}" -C "${TARGET}"

# A template that carries credentials from a previous config.sh run would be
# cloned into every instance, and the runners would fight over one identity.
rm -f "${TARGET}/.runner" "${TARGET}/.credentials" "${TARGET}/.credentials_rsaparams"

# Gatekeeper quarantines everything downloaded with a browser or curl. Clearing
# it here means jobs don't hit "cannot be opened because the developer cannot be
# verified" on the runner's own binaries.
xattr -dr com.apple.quarantine "${TARGET}" 2>/dev/null || true

# Verify the clone strategy actually works on this volume. On APFS this is a
# copy-on-write clone: near-instant and almost free in disk, which is what makes
# a fresh runner per job practical.
PROBE="$(mktemp -d)/probe"
if cp -Rc "${TARGET}/" "${PROBE}" 2>/dev/null; then
  CLONE="copy-on-write (APFS clone)"
else
  cp -R "${TARGET}/" "${PROBE}"
  CLONE="full copy — the volume does not support cloning, so each runner will
  cost a real copy of $(du -sh "${TARGET}" | cut -f1). Consider moving the
  template and instances directory to an APFS volume."
fi
rm -rf "$(dirname "${PROBE}")"

cat <<EOF

Template ready: ${TARGET}
Per-instance copy strategy: ${CLONE}

Add this pool to your arc.yaml:

  - name: macos
    labels: [self-hosted, macos, arm64]
    provider: process
    min: 1
    max: 4
    process:
      template_dir: ${TARGET}

Then run: arc doctor
EOF
