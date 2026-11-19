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
# Xcode Command Line Tools, gh logged in with repo access (also to the
# Homebrew tap repository, see the end of this script), and the update-feed
# signing key (see "Update feed" below).
#
# macOS signing: with APPLE_SIGNING_IDENTITY (or APPLE_CERTIFICATE) in the
# environment viva signs with that Developer ID, and with notary credentials
# (APPLE_NOTARY_PROFILE, …) also notarizes. Without them the bundles are signed
# ad hoc: Gatekeeper still asks on first open, but the signature is valid, so
# macOS offers "Open Anyway" instead of calling the app damaged, and the in-app
# updater (which checks the signature) can install the next version.
#
# Update feed: the app's updater reads latest.json and latest.json.sig from the
# latest release. They are signed with the Ed25519 private key that matches
# one of update.PublicKeys: $VIVA_UPDATE_KEY (the key text), or the file named by
# $GENIMAGE_UPDATE_KEY_FILE (default ~/.genimage-release/update.key, made by
# `viva update-keygen`). Never commit that key.
set -euo pipefail

cd "$(dirname "$0")/.."
VIVA="${VIVA:-viva}"
command -v "$VIVA" >/dev/null || VIVA="$(go env GOPATH)/bin/viva"

VERSION="$(sed -n 's/^const Version = "\(.*\)"/\1/p' internal/version/version.go)"
REPO="$(sed -n 's/^const Repo = "\(.*\)"/\1/p' internal/version/version.go)"
TAG="v${VERSION}"
echo "==> Releasing ${TAG}"

# Find the feed key before building, so a missing key fails fast.
KEY_ARGS=()
if [[ -z "${VIVA_UPDATE_KEY:-}" ]]; then
  KEY_FILE="${GENIMAGE_UPDATE_KEY_FILE:-$HOME/.genimage-release/update.key}"
  if [[ ! -f "$KEY_FILE" ]]; then
    echo "error: no update-feed signing key: set VIVA_UPDATE_KEY or create $KEY_FILE with" >&2
    echo "       $VIVA update-keygen -private \"$KEY_FILE\" -public \"\"" >&2
    exit 1
  fi
  KEY_ARGS=(-key "$KEY_FILE")
fi

# mac_package runs viva package for macOS with the given flags. It signs ad hoc
# unless a Developer ID identity or certificate is in the environment, in which
# case viva picks that up (and notarizes when notary credentials are set too).
mac_package() {
  if [[ -z "${APPLE_SIGNING_IDENTITY:-}${APPLE_CERTIFICATE:-}" ]]; then
    "$VIVA" package --sign-identity - "$@"
  else
    "$VIVA" package "$@"
  fi
}

go vet ./...
go test -count=1 ./...

NOTES="$(mktemp)"
if [[ -f build/RELEASE_NOTES.md ]]; then cp build/RELEASE_NOTES.md "$NOTES"; fi
rm -rf build
mkdir -p build

mac_package --arch arm64 --format zip,dmg
mac_package --arch amd64 --format zip,dmg
"$VIVA" package --target windows --arch amd64 --format zip
"$VIVA" package --target windows --arch arm64 --format zip

# The signed update feed: it lists every zip with its size and SHA-256 and
# points at this release's download URLs.
"$VIVA" feed ${KEY_ARGS[@]+"${KEY_ARGS[@]}"} \
  -base-url "https://github.com/${REPO}/releases/download/${TAG}" \
  -notes "https://github.com/${REPO}/releases/tag/${TAG}"

(cd build && shasum -a 256 genimage-*.zip genimage-*.dmg > SHA256SUMS.txt)

# The Homebrew cask for this release, pushed to the tap after publishing.
scripts/brew-cask.sh "$VERSION" build "$REPO" > build/genimage-mcp.rb
ls -la build

if [[ "${1:-}" == "--dry-run" ]]; then
  echo "==> Dry run: not publishing"
  exit 0
fi

if [[ ! -s "$NOTES" ]]; then
  echo "GenImage MCP ${TAG}" > "$NOTES"
fi
gh release create "$TAG" build/genimage-*.zip build/genimage-*.dmg build/SHA256SUMS.txt \
  build/latest.json build/latest.json.sig \
  --title "GenImage MCP ${TAG}" --notes-file "$NOTES"
rm -f "$NOTES"

# Update the Homebrew tap (brew install --cask <owner>/tap/genimage-mcp). The
# tap repository is <owner>/homebrew-tap unless GENIMAGE_TAP_REPO names another;
# when it does not exist the release stays published and the cask is left in
# build/ to push by hand.
TAP_REPO="${GENIMAGE_TAP_REPO:-${REPO%%/*}/homebrew-tap}"
if gh repo view "$TAP_REPO" >/dev/null 2>&1; then
  TAP_DIR="$(mktemp -d)"
  gh repo clone "$TAP_REPO" "$TAP_DIR" -- --depth 1 --quiet
  mkdir -p "$TAP_DIR/Casks"
  cp build/genimage-mcp.rb "$TAP_DIR/Casks/genimage-mcp.rb"
  git -C "$TAP_DIR" add Casks/genimage-mcp.rb
  if git -C "$TAP_DIR" diff --cached --quiet; then
    echo "==> Tap ${TAP_REPO} already has genimage-mcp ${VERSION}"
  else
    git -C "$TAP_DIR" commit --quiet -m "genimage-mcp ${VERSION}"
    git -C "$TAP_DIR" push --quiet origin HEAD # HEAD also works for a tap that has no commits yet
    echo "==> Updated tap ${TAP_REPO}: genimage-mcp ${VERSION}"
  fi
  rm -rf "$TAP_DIR"
else
  echo "warning: tap repository ${TAP_REPO} not found; create it and push build/genimage-mcp.rb as Casks/genimage-mcp.rb" >&2
fi
