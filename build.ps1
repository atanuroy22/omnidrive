# Cross-compile OmniDrive from Windows.
#
#   .\build.ps1              # every target
#   .\build.ps1 android      # just the Android binaries
#   .\build.ps1 android-arm64
#
# All output is CGO-free and statically linked.

param([string[]]$Targets = @('all'))

$ErrorActionPreference = 'Stop'

$Version = $env:VERSION
if (-not $Version) {
    try { $Version = (git describe --tags --always --dirty 2>$null) } catch { }
    if (-not $Version) { $Version = 'dev' }
}
$Out = 'build'
$LdFlags = "-s -w -X main.version=$Version"

# Android runs standard static Linux ELF binaries; GOOS=linux avoids needing
# the NDK and produces the most portable result.
$All = [ordered]@{
    'android-arm64'  = @('linux', 'arm64', '')
    'android-arm'    = @('linux', 'arm', '7')
    'android-x86_64' = @('linux', 'amd64', '')
    'linux-amd64'    = @('linux', 'amd64', '')
    'linux-arm64'    = @('linux', 'arm64', '')
    'windows-amd64'  = @('windows', 'amd64', '')
    'darwin-arm64'   = @('darwin', 'arm64', '')
    'darwin-amd64'   = @('darwin', 'amd64', '')
}

function Build-One([string]$Name) {
    $spec = $All[$Name]
    if (-not $spec) {
        throw "unknown target: $Name (available: $($All.Keys -join ', '))"
    }
    $goos, $goarch, $goarm = $spec

    $file = Join-Path $Out "omnidrive-$Name"
    if ($goos -eq 'windows') { $file += '.exe' }

    "  {0,-18} {1}/{2}" -f $Name, $goos, $goarch | Write-Host

    $env:CGO_ENABLED = '0'
    $env:GOOS = $goos
    $env:GOARCH = $goarch
    if ($goarm) { $env:GOARM = $goarm } else { Remove-Item env:GOARM -ErrorAction SilentlyContinue }

    go build -trimpath -ldflags $LdFlags -o $file ./cmd/omnidrive
    if ($LASTEXITCODE -ne 0) { throw "build failed for $Name" }
}

New-Item -ItemType Directory -Force $Out | Out-Null
Write-Host "OmniDrive $Version"

$selected = switch ($Targets[0]) {
    'all'     { $All.Keys }
    'android' { @('android-arm64', 'android-arm', 'android-x86_64') }
    default   { $Targets }
}

try {
    foreach ($t in $selected) { Build-One $t }
}
finally {
    # Leave the shell's Go environment as we found it.
    foreach ($v in 'CGO_ENABLED', 'GOOS', 'GOARCH', 'GOARM') {
        Remove-Item "env:$v" -ErrorAction SilentlyContinue
    }
}

Write-Host ''
Get-ChildItem $Out | Select-Object Name, @{n = 'Size'; e = { '{0:N1} MB' -f ($_.Length / 1MB) } } | Format-Table -AutoSize
Write-Host 'Copy the android-* binary to your phone and follow scripts/install-termux.sh'
