#!/usr/bin/env bash
# Coverage ratchet.
#
# This repo publishes fast — 20 releases in about 30 hours at one point — and
# that is a deliberate choice. The risk it carries is that coverage erodes one
# hurried PR at a time and nobody notices until a regression ships. This gate
# makes the erosion loud: floors only ever move UP, and the numbers live in
# coverage-floors.txt where raising them is a visible line in a diff.
#
# Floors are set at the CURRENT measured value, not at an aspiration. A failure
# means this change removed coverage that existed before it — not that the repo
# fell short of some ideal.
#
# Two legitimate sources of drops have nothing to do with tests:
#
#   - Statement counting differs between Go toolchains, so a version bump can
#     move a package by a tenth of a point.
#   - Coverage differs by PLATFORM. cmd/ contains GOOS-specific code, so the
#     same commit measures 59.9% on Windows and 59.6% on Linux.
#
# Because of the second one, THE FLOORS ARE THE CI (linux/amd64) NUMBERS. If you
# regenerate them on Windows or macOS you will encode values CI cannot meet, and
# the gate will fail for everyone. Regenerate on Linux, or take the numbers from
# the failing CI log -- the gate prints its full measured table on failure for
# exactly this reason.
#
# --allow-lower is for these cases. Before reaching for it, check that no
# CRITICAL FUNCTION moved: a package total shifting 0.1 is noise, dispatchChain
# falling is not.
#
#   ./scripts/coverage-gate.sh                          # check
#   ./scripts/coverage-gate.sh --update                 # raise floors to today
#   ./scripts/coverage-gate.sh --update --allow-lower   # ... and let floors DROP
#   ./scripts/coverage-gate.sh --update --allow-remove  # ... and let entries vanish
#
# --update on its own can only RAISE. Lowering or removing a floor needs an
# explicit flag, because the easy way to defeat a ratchet is not to argue with
# it -- it is to regenerate it in the same PR and let the numbers slide past as
# noise. The flags force that to be a deliberate act with a name on it.
set -euo pipefail

cd "$(dirname "$0")/.."
FLOORS="scripts/coverage-floors.txt"
PROFILE="${COVERAGE_PROFILE:-coverage.out}"
# Only packages that carry tests. ./... would also pull in tools/ and test/load/,
# which have no tests to measure and only add noise (and, on a machine with two
# Go installs on PATH, a toolchain-mismatch failure that has nothing to do with
# coverage).
PKGS="${COVERAGE_PKGS:-./internal/... ./cmd/...}"

# shellcheck disable=SC2086
go test $PKGS -coverprofile="$PROFILE" >/dev/null

measured=$(mktemp)
trap 'rm -f "$measured"' EXIT

# Per-package totals.
# shellcheck disable=SC2086
go test $PKGS -cover 2>/dev/null \
  | awk '$1 == "ok" && /coverage: [0-9.]+% of statements/ {
      for (i = 1; i <= NF; i++) if ($i == "coverage:") { gsub(/%/, "", $(i+1)); print "pkg " $2 " " $(i+1) }
    }' >> "$measured"

# Named critical-path functions. A package total can stay flat while the
# function every request passes through loses its tests, so these are pinned by
# name rather than trusted to the average.
go tool cover -func="$PROFILE" \
  | awk '$1 != "total:" { gsub(/%/, "", $NF); split($1, p, ":"); print "func " $2 " " $NF }' \
  | sort -u >> "$measured"

ALLOW_LOWER=0
ALLOW_REMOVE=0
UPDATE=0
for arg in "$@"; do
  case "$arg" in
    --update)       UPDATE=1 ;;
    --allow-lower)  ALLOW_LOWER=1 ;;
    --allow-remove) ALLOW_REMOVE=1 ;;
    *) echo "unknown flag: $arg" >&2; exit 2 ;;
  esac
done

# Only the critical path is pinned per-function. Pinning everything would turn
# every refactor into a floors-file edit and train people to ignore the file.
CRITICAL='^func (RouteRequestWithProvider|RouteRequest|dispatchChain|executeAttempt|streamProxyResponse|streamCopy|awaitFirstStreamEvent|setServedModelHeaders|planModelAttempts|applyCapabilityFilter|filterAvailableAttempts|rankAttemptsByScore) '

if [[ $UPDATE -eq 1 ]]; then
  proposed=$(mktemp)
  trap 'rm -f "$measured" "$proposed"' EXIT
  { grep '^pkg ' "$measured" | sort; grep -E "$CRITICAL" "$measured" | sort; } > "$proposed"

  if [[ "$(go env GOOS)/$(go env GOARCH)" != "linux/amd64" ]]; then
    echo "REFUSING to regenerate floors on $(go env GOOS)/$(go env GOARCH)." >&2
    echo "  The floors are the CI (linux/amd64) numbers; package totals differ by" >&2
    echo "  platform, so writing them here would encode values CI cannot meet." >&2
    echo "  Regenerate on Linux, or copy the measured table the gate prints when" >&2
    echo "  it fails in CI." >&2
    exit 1
  fi

  refused=0
  if [[ -f "$FLOORS" ]]; then
    while read -r kind name floor; do
      [[ -z "${kind:-}" || "$kind" == \#* ]] && continue
      now=$(awk -v k="$kind" -v n="$name" '$1 == k && $2 == n { print $3; exit }' "$proposed")
      if [[ -z "$now" ]]; then
        if [[ $ALLOW_REMOVE -eq 0 ]]; then
          echo "REFUSING to drop the floor for $kind $name (was $floor%)." >&2
          echo "  It no longer reports coverage. If that is correct, pass --allow-remove." >&2
          refused=1
        fi
        continue
      fi
      if awk -v a="$now" -v b="$floor" 'BEGIN { exit !(a + 0.001 < b) }' && [[ $ALLOW_LOWER -eq 0 ]]; then
        echo "REFUSING to lower $kind $name: $floor% -> $now%. Pass --allow-lower if intended." >&2
        refused=1
      fi
    done < "$FLOORS"
  fi
  if [[ $refused -ne 0 ]]; then
    echo "" >&2
    echo "Nothing written. A ratchet that regenerates downward is not a ratchet." >&2
    exit 1
  fi

  {
    echo "# Coverage floors — generated by scripts/coverage-gate.sh --update."
    echo "# --update can only RAISE these. Lowering needs --allow-lower and removing"
    echo "# needs --allow-remove, so either one shows up as an argued change."
    cat "$proposed"
  } > "$FLOORS"
  echo "floors written to $FLOORS"
  exit 0
fi

if [[ ! -f "$FLOORS" ]]; then
  echo "no $FLOORS; run: ./scripts/coverage-gate.sh --update" >&2
  exit 1
fi

# The floors are the CI numbers (linux/amd64). Elsewhere, PACKAGE TOTALS are
# advisory and FUNCTION floors still bind.
#
# That split is not a convenience, it is what the measurements show. The same
# commit reports cmd/ at 59.6% on Linux and 59.9% on Windows, and
# internal/router at 81.8% vs 81.7% — GOOS-specific code shifts the totals in
# both directions. Every pinned FUNCTION measured identically on both. So the
# per-function floors are portable and stay hard everywhere; a package total off
# by a tenth on a developer's laptop is noise, and failing on it would train
# people to skip the gate — which costs more than the total was ever worth.
HARD_PKG=1
if [[ "$(go env GOOS)/$(go env GOARCH)" != "linux/amd64" ]]; then
  HARD_PKG=0
fi

fail=0
while read -r kind name floor; do
  [[ -z "${kind:-}" || "$kind" == \#* ]] && continue
  matches=$(awk -v k="$kind" -v n="$name" '$1 == k && $2 == n' "$measured" | wc -l | tr -d ' ')
  if [[ "$matches" -gt 1 ]]; then
    # Floors key on a bare function name, so two same-named functions in
    # different files would silently compare against whichever sorted first.
    echo "AMBIGUOUS  $kind $name matches $matches measured entries."
    echo "           Rename one, or drop it from $FLOORS — a floor that compares"
    echo "           against an arbitrary one of them protects nothing."
    fail=1
    continue
  fi
  now=$(awk -v k="$kind" -v n="$name" '$1 == k && $2 == n { print $3; exit }' "$measured")
  if [[ -z "$now" ]]; then
    echo "GONE  $kind $name — floor $floor%, but it no longer reports coverage."
    echo "      If it was renamed or removed on purpose, update $FLOORS in this PR."
    fail=1
    continue
  fi
  # 0.001, not a slack budget: coverage reports one decimal, so this absorbs
  # float representation only. The 0.05 this replaced quietly allowed real drops.
  if awk -v a="$now" -v b="$floor" 'BEGIN { exit !(a + 0.001 < b) }'; then
    if [[ "$kind" == "pkg" && $HARD_PKG -eq 0 ]]; then
      echo "note  $kind $name: $now% < floor $floor% (advisory off linux/amd64)"
    else
      echo "DROP  $kind $name: $now% < floor $floor%"
      fail=1
    fi
  fi
done < "$FLOORS"

if [[ $fail -ne 0 ]]; then
  # Print everything measured, not just the failures. Coverage varies by
  # platform (GOOS-specific code in cmd/) and by toolchain (statement counting
  # changes between Go versions), so whoever has to fix this needs the numbers
  # from the machine that failed -- which is usually not the machine they are on.
  echo "" >&2
  echo "Measured on $(go env GOOS)/$(go env GOARCH), $(go version | awk '{print $3}'):" >&2
  # Only the entries that are actually compared. Dumping every function produced
  # 300 lines of log in which the four that mattered were invisible.
  { grep '^pkg ' "$measured"; grep -E "$CRITICAL" "$measured"; } | sed 's/^/  /' >&2
  cat >&2 <<'MSG'

Coverage went down. Add the tests, or — if the drop is correct (code deleted,
function renamed) — run ./scripts/coverage-gate.sh --update and let the changed
floors show up in the diff, where a reviewer can see them.
MSG
  exit 1
fi

echo "coverage gate: OK"
