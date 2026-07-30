#!/usr/bin/env bash
# Query a free model through CervoProxy (OpenRouter :free, OpenAI-compatible).
#
# Config (all optional — the only requirement is a reachable proxy):
#   CERVOPROXY_URL      base URL of the proxy      (default http://127.0.0.1:8080)
#   CERVOPROXY_BIN      path to the cervoproxy binary. If set and the proxy is
#                       down, it is started on-demand (with idle self-shutdown).
#   OPENROUTER_API_KEY  used only when THIS script has to start the proxy;
#                       when the proxy is already running it holds its own key.
#
# Usage:  ask.sh <profile> <prompt...>        |   echo "<prompt>" | ask.sh <profile>
# Profiles: coding (default) | reasoning | simple | creative | vision
set -uo pipefail

URL="${CERVOPROXY_URL:-http://127.0.0.1:8080}"
PY=python; command -v python >/dev/null 2>&1 || PY=python3

profile="${1:-coding}"; shift || true
prompt="$*"
if [ -z "$prompt" ] && [ ! -t 0 ]; then prompt="$(cat)"; fi
if [ -z "$prompt" ]; then echo "usage: ask.sh <profile> <prompt>" >&2; exit 2; fi

up() { curl -s -m 2 -o /dev/null "$URL/health" 2>/dev/null; }

# On-demand start (best effort) when a local binary is provided.
if ! up && [ -n "${CERVOPROXY_BIN:-}" ] && [ -e "${CERVOPROXY_BIN}" ]; then
  export PORT="${PORT:-8080}" GRPC_PORT="${GRPC_PORT:-19090}" OTEL_ENABLED=false PROXY_IDLE_TIMEOUT="${PROXY_IDLE_TIMEOUT:-20m}"
  case "$(uname -s)" in
    MINGW*|MSYS*|CYGWIN*)
      powershell.exe -NoProfile -Command "Start-Process -FilePath '${CERVOPROXY_BIN}' -WindowStyle Hidden" >/dev/null 2>&1 || true
      ;;
    *)
      nohup "${CERVOPROXY_BIN}" >/dev/null 2>&1 & disown 2>/dev/null || true
      ;;
  esac
  for _ in $(seq 1 20); do up && break; sleep 0.5; done
fi

if ! up; then
  echo "ERROR: CervoProxy not reachable at ${URL}." >&2
  echo "  Start it (see the repo README) or set CERVOPROXY_URL / CERVOPROXY_BIN." >&2
  exit 1
fi

body="$("$PY" -c 'import json,sys;print(json.dumps({"model":sys.argv[1],"messages":[{"role":"user","content":sys.argv[2]}]}))' "$profile" "$prompt")"

curl -s -m 180 "${URL}/v1/chat/completions" \
  -H "Authorization: Bearer dummy" -H "Content-Type: application/json" \
  -d "$body" \
| "$PY" -c 'import json,sys
try:
    d=json.load(sys.stdin)
except Exception as e:
    print("ERROR: no/invalid response from CervoProxy:", e); sys.exit(1)
if "choices" in d:
    print((d["choices"][0]["message"].get("content") or "").strip())
    print("\n[model: %s]" % d.get("model",""), file=sys.stderr)
else:
    print("ERROR:", json.dumps(d.get("error", d))[:500]); sys.exit(1)'
