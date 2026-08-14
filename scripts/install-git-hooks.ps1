$ErrorActionPreference = 'Stop'
git config core.hooksPath .githooks
Write-Host 'CalvoProxy quality hooks installed: pre-commit and pre-push.'
