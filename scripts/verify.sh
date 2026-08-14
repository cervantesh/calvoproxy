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

# Tests must not inherit the operator's own configuration. The proxy reads its
# behaviour from the environment, so a developer shell exporting a real setting
# silently changes what the code under test does: an exported PROXY_ADMIN_TOKEN
# makes admin() see a configured token and answer 401 where TestAdminGate
# expects the open, no-token path, failing a suite that is green in CI. Tests
# that need a value set it themselves with t.Setenv. Strip anything that looks
# like proxy configuration or a provider credential, and let the value the test
# chooses be the only one it sees.
sanitized_env=()
sanitized_names=""
while IFS= read -r name; do
  case "$name" in
    PROXY_*|CERVO_*|CALVOPROXY_*|HERMES_*|*_API_KEY|*_API_TOKEN)
      sanitized_env+=(-u "$name")
      sanitized_names="$sanitized_names $name"
      ;;
  esac
done < <(compgen -e)
if [ -n "$sanitized_names" ]; then
  # Names only, never values: this output lands in terminals and CI logs.
  printf 'test env: ignoring inherited%s\n' "$sanitized_names"
fi
gotest() { env ${sanitized_env[@]+"${sanitized_env[@]}"} go test "$@"; }

gotest ./...
if [[ "${1:-}" == "--push" ]]; then
  gotest -race ./...
fi
