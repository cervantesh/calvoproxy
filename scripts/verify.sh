#!/usr/bin/env bash
# Reproducible local/CI quality gate. Tools are version-pinned deliberately.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

GO_BIN="$(go env GOPATH)/bin"
ensure() { command -v "$1" >/dev/null 2>&1 || go install "$2"; }
ensure staticcheck honnef.co/go/tools/cmd/staticcheck@v0.6.1
ensure actionlint github.com/rhysd/actionlint/cmd/actionlint@v1.7.9
ensure govulncheck golang.org/x/vuln/cmd/govulncheck@v1.6.0
export PATH="$GO_BIN:$PATH"

unformatted=""
while IFS= read -r -d '' f; do
  if [ -n "$(tr -d '\r' < "$f" | gofmt -l)" ]; then
    unformatted="$unformatted$f"$'\n'
  fi
done < <(git ls-files -z '*.go' -- ':!vendor')
if [ -n "$unformatted" ]; then
  printf 'gofmt needed for:\n%s' "$unformatted" >&2
  exit 1
fi
go vet ./...
staticcheck ./...
govulncheck ./...
actionlint
./scripts/vendor-manifest.sh
go test ./...
if [[ "${1:-}" == "--push" ]]; then
  go test -race ./...
fi
