#!/usr/bin/env bash
# Build every release package and publish them as a GitHub release.
#
#   scripts/release.sh            build, test and publish v<version>
#   scripts/release.sh --dry-run  build and test only
#
# Before running: bump the version in internal/version/version.go and viva.toml
# (a test fails if they differ), write release notes to
# build/RELEASE_NOTES.md (optional), commit and push.
#
# Needs: Go, viva CLI (go install github.com/vivaui/viva/cmd/viva@v0.6.5),
# Xcode Command Line Tools, and gh logged in with repo access.
set -euo pipefail

cd "$(dirname "$0")/.."
VIVA="${VIVA:-viva}"
command -v "$VIVA" >/dev/null || VIVA="$(go env GOPATH)/bin/viva"

VERSION="$(sed -n 's/^const Version = "\(.*\)"/\1/p' internal/version/version.go)"
TAG="v${VERSION}"
echo "==> Releasing ${TAG}"

go vet ./...
go test -count=1 ./...

NOTES="$(mktemp)"
if [[ -f build/RELEASE_NOTES.md ]]; then cp build/RELEASE_NOTES.md "$NOTES"; fi
rm -rf build
mkdir -p build

"$VIVA" package --arch arm64 --format zip,dmg
"$VIVA" package --arch amd64 --format zip,dmg
"$VIVA" package --target windows --arch amd64 --format zip
"$VIVA" package --target windows --arch arm64 --format zip

(cd build && shasum -a 256 genimage-*.zip genimage-*.dmg > SHA256SUMS.txt)
ls -la build

if [[ "${1:-}" == "--dry-run" ]]; then
  echo "==> Dry run: not publishing"
  exit 0
fi

if [[ ! -s "$NOTES" ]]; then
  echo "GenImage MCP ${TAG}" > "$NOTES"
fi
gh release create "$TAG" build/genimage-*.zip build/genimage-*.dmg build/SHA256SUMS.txt \
  --title "GenImage MCP ${TAG}" --notes-file "$NOTES"
rm -f "$NOTES"
