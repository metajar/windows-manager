#!/usr/bin/env bash
# Build the Windows agent MSI from rewardd-agent.wxs using the WiX dotnet tool.
#
# WiX requires Windows (it calls native Windows APIs). CI builds the MSI on a
# windows-latest runner; cross-compiled binaries are produced on Linux first.
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

# Pin WiX 5.x: v7 requires an OSMF EULA acceptance step and does not build on
# Linux. MSI builds must run on a Windows host (see .github/workflows/release.yml).
WIX_VERSION="${WIX_VERSION:-5.0.2}"

if ! command -v dotnet >/dev/null 2>&1; then
  echo "error: the .NET SDK (dotnet) is required to build the MSI." >&2
  echo "       Install from https://dotnet.microsoft.com/download then re-run." >&2
  exit 1
fi

export DOTNET_ROOT="${DOTNET_ROOT:-$(dirname "$(dirname "$(command -v dotnet)")")}"
export PATH="$PATH:$HOME/.dotnet/tools"

# Install (or pin) the wix tool. Uninstall first when the wrong major is present
# so `dotnet tool install --version` is not rejected.
installed_wix="$(dotnet tool list --global 2>/dev/null | awk '/^wix /{print $2; exit}')"
if [[ "$installed_wix" != "$WIX_VERSION" ]]; then
  echo "==> installing wix dotnet tool $WIX_VERSION (was: ${installed_wix:-none})"
  if [[ -n "$installed_wix" ]]; then
    dotnet tool uninstall --global wix
  fi
  dotnet tool install --global wix --version "$WIX_VERSION"
fi

echo "==> ensuring WixToolset.Util.wixext extension"
wix extension remove -g WixToolset.Util.wixext 2>/dev/null || true
wix extension add -g WixToolset.Util.wixext

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
