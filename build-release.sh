#!/bin/bash
# Builds the release assets into dist/: macOS binaries (arm64 and amd64), install.sh and SHA256SUMS.
# Attach all of dist/* to a GitHub release of mishka81/simple-mock-http-server, e.g.:
#   gh release create v1.0.0 dist/* --repo mishka81/simple-mock-http-server --title v1.0.0
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"
export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"
DIST="${DIST:-dist}"

go vet ./...
go test ./...

rm -rf "$DIST"
mkdir -p "$DIST"
for arch in arm64 amd64; do
    echo "Building shttpmocksrv-darwin-$arch"
    CGO_ENABLED=0 GOOS=darwin GOARCH="$arch" go build -trimpath -ldflags "-s -w" -o "$DIST/shttpmocksrv-darwin-$arch" .
done

cp install.sh "$DIST/install.sh"
chmod +x "$DIST/install.sh"

(cd "$DIST" && shasum -a 256 shttpmocksrv-darwin-* >SHA256SUMS)

echo
cat "$DIST/SHA256SUMS"
