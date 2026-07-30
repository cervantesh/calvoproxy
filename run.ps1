# Runs CalvoProxy locally. The real OpenRouter key lives here (server-side);
# callers send only a dummy key.
# Set it once (user scope):  setx OPENROUTER_API_KEY "sk-or-v1-..."
param(
  [int]$Port = 8080,
  [int]$GrpcPort = 19090
)

# Repo root = this script's directory (works wherever the repo is cloned).
$root = if ($PSScriptRoot) { $PSScriptRoot } else { (Get-Location).Path }
Set-Location $root

if (-not $env:OPENROUTER_API_KEY) {
  $env:OPENROUTER_API_KEY = [Environment]::GetEnvironmentVariable('OPENROUTER_API_KEY','User')
}
if (-not $env:OPENROUTER_API_KEY) {
  Write-Warning "OPENROUTER_API_KEY is not set — upstream OpenRouter calls will 401."
}

$env:PORT = "$Port"
$env:GRPC_PORT = "$GrpcPort"
$env:OTEL_ENABLED = "false"   # no local jaeger/OTLP collector; keep logs clean

$bin = Join-Path $root 'calvoproxy.exe'
if (-not (Test-Path $bin)) {
  Write-Host "Building calvoproxy.exe ..."
  & go build -o $bin ./cmd
}

Write-Host "CalvoProxy on http://localhost:$Port  (health: /health)"
& $bin
