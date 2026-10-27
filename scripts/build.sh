#!/usr/bin/env bash
# Cross-compile the Go arkkvm-mcp into bin/ using the
# arkkvm-mcp-<os>-<arch>[.exe] naming scheme; the host platform
# additionally gets an unversioned bin/arkkvm-mcp alias.
set -euo pipefail

# proxy.golang.org is unreachable from CN build hosts; default to a reachable
# mirror, but let callers override by exporting GOPROXY themselves.
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"

cd "$(dirname "$0")/.."

if [ "$#" -gt 0 ]; then
  TARGETS=("$@")
else
  TARGETS=(darwin-arm64 darwin-amd64 linux-arm64 linux-amd64 windows-arm64 windows-amd64)
fi

# CI can export FIRMWARE_VERSION to stamp the build; the version is injected
# at link time (like the firmware app's version), via -X. Falls back to the
# dev default when unset.
VERSION_LDFLAG=""
if [[ -n "${FIRMWARE_VERSION:-}" ]]; then
  VERSION_LDFLAG="-X arkkvm-mcp/internal/server.Version=${FIRMWARE_VERSION}"
  echo "== version: ${FIRMWARE_VERSION} =="
fi

for target in "${TARGETS[@]}"; do
  os="${target%%-*}"
  arch="${target#*-}"
  # Accept common aliases (win-x64) alongside GOOS/GOARCH names.
  [[ "$os" == "win" ]] && os="windows"
  [[ "$arch" == "x64" ]] && arch="amd64"
  [[ "$arch" == "x86" || "$arch" == "ia32" ]] && arch="386"
  ext=""
  [[ "$os" == "windows" ]] && ext=".exe"
  out="bin/arkkvm-mcp-${os}-${arch}${ext}"
  echo "== building ${out} =="
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w ${VERSION_LDFLAG}" \
    -o "$out" ./cmd/arkkvm-mcp
done

host_os="$(go env GOOS)"
host_arch="$(go env GOARCH)"
host_ext=""
[[ "$host_os" == "windows" ]] && host_ext=".exe"
if [[ -f "bin/arkkvm-mcp-${host_os}-${host_arch}${host_ext}" ]]; then
  cp "bin/arkkvm-mcp-${host_os}-${host_arch}${host_ext}" bin/arkkvm-mcp
  echo "== host alias bin/arkkvm-mcp updated =="
fi

# macOS kills unsigned local binaries with SIGKILL (Code Signature Invalid)
# when Cursor/spawner launches them — ad-hoc sign every darwin artifact.
if command -v codesign >/dev/null 2>&1; then
  for f in bin/arkkvm-mcp-darwin-*; do
    [[ -f "$f" ]] || continue
    codesign -s - --force "$f"
  done
  if [[ -f bin/arkkvm-mcp ]]; then
    codesign -s - --force bin/arkkvm-mcp
  fi
  echo "== darwin ad-hoc codesign done =="
fi
