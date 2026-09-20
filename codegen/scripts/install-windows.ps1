[CmdletBinding()]
param(
    [switch]$SkipInstall
)

$ErrorActionPreference = "Stop"

$roostGoBin = (& go env GOBIN).Trim()
if ($LASTEXITCODE -ne 0) {
    throw "go env GOBIN failed with exit code $LASTEXITCODE"
}

if ([string]::IsNullOrWhiteSpace($roostGoBin)) {
    $roostGoPath = (& go env GOPATH).Trim()
    if ($LASTEXITCODE -ne 0) {
        throw "go env GOPATH failed with exit code $LASTEXITCODE"
    }
    if ([string]::IsNullOrWhiteSpace($roostGoPath)) {
        throw "both GOBIN and GOPATH are empty"
    }

    # go install uses the first GOPATH entry when GOBIN is not configured.
    $roostGoPath = ($roostGoPath -split [IO.Path]::PathSeparator)[0]
    $roostGoBin = Join-Path $roostGoPath "bin"
}

New-Item -ItemType Directory -Path $roostGoBin -Force | Out-Null

# Keep the existing GOBIN/GOPATH unchanged. Preserve every existing user PATH
# entry and append the directory actually used by go install only when missing.
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
$entries = @($userPath -split ";" | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
$alreadyPresent = $entries | Where-Object {
    $_.Trim().TrimEnd("\") -ieq $roostGoBin.TrimEnd("\")
}
if (-not $alreadyPresent) {
    $entries += $roostGoBin
    [Environment]::SetEnvironmentVariable("Path", ($entries -join ";"), "User")
}

# Also refresh this PowerShell process for the verification below.
if (-not (($env:Path -split ";") | Where-Object { $_.TrimEnd("\") -ieq $roostGoBin.TrimEnd("\") })) {
    $env:Path += ";$roostGoBin"
}

if (-not $SkipInstall) {
    & go install ./cmd/roost
    if ($LASTEXITCODE -ne 0) {
        throw "go install ./cmd/roost failed with exit code $LASTEXITCODE"
    }
}

$command = Get-Command roost -ErrorAction Stop
Write-Host "roost installed: $($command.Source)"
Write-Host "Go bin added to user PATH: $roostGoBin"
& roost help
