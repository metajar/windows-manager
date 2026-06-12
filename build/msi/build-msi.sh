#!/usr/bin/env bash
# Build the Windows agent MSI from rewardd-agent.wxs using the WiX dotnet tool.
#
# WiX v4/v5 is cross-platform: this runs on Linux, macOS or Windows as long as
# the .NET SDK is installed. CI uses it on a Linux runner.
#
# Inputs (env):
#   VERSION    MSI ProductVersion, e.g. 1.2.3   (default: 0.0.0)
#   AGENT_EXE  path to the windows/amd64 agent  (default: ../../dist/rewardd-agent-windows-amd64.exe)
#   OUT        output .msi path                  (default: ../../dist/rewardd-agent-<VERSION>.msi)
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$here"

VERSION="${VERSION:-0.0.0}"
# MSI versions must be numeric x.y.z; strip a leading v and any -dirty/-gN suffix.
MSI_VERSION="$(echo "${VERSION#v}" | sed -E 's/[-+].*$//; s/[^0-9.].*$//')"
MSI_VERSION="${MSI_VERSION:-0.0.0}"

AGENT_EXE="${AGENT_EXE:-../../dist/rewardd-agent-windows-amd64.exe}"
OUT="${OUT:-../../dist/rewardd-agent-${VERSION}.msi}"

if ! command -v dotnet >/dev/null 2>&1; then
  echo "error: the .NET SDK (dotnet) is required to build the MSI." >&2
  echo "       Install from https://dotnet.microsoft.com/download then re-run." >&2
  exit 1
fi

# Install the wix tool + Util extension locally (idempotent).
if ! dotnet tool list --global 2>/dev/null | grep -qi '^wix '; then
  echo "==> installing wix dotnet tool (global)"
  dotnet tool install --global wix >/dev/null 2>&1 || dotnet tool update --global wix >/dev/null
fi
export PATH="$PATH:$HOME/.dotnet/tools"

echo "==> ensuring WixToolset.Util.wixext extension"
wix extension add -g WixToolset.Util.wixext >/dev/null 2>&1 || true

if [[ ! -f "$AGENT_EXE" ]]; then
  echo "error: agent exe not found at '$AGENT_EXE'. Build it first ('make agent')." >&2
  exit 1
fi

mkdir -p "$(dirname "$OUT")"

echo "==> building MSI version $MSI_VERSION -> $OUT"
wix build rewardd-agent.wxs \
  -arch x64 \
  -ext WixToolset.Util.wixext \
  -d "Version=$MSI_VERSION" \
  -d "AgentExe=$AGENT_EXE" \
  -o "$OUT"

echo "built $OUT"
