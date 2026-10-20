param(
    [string]$Root = (Resolve-Path "$PSScriptRoot/..")
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

Push-Location $Root
try {
    Write-Host "[integration] running go tests with integration tag"
    go test -tags integration ./test/...
}
finally {
    Pop-Location
}
