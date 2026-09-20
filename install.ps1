# BE-Code installer — Windows (PowerShell).
#
#   .\install.cmd              install to %LOCALAPPDATA%\Programs\be-code and add to user PATH
#   .\install.cmd -NoSetup     skip the first-run setup wizard
#
# install.cmd is a launcher for this script. Run directly, .\install.ps1 is
# refused on a default Windows ("running scripts is disabled on this system");
# the launcher — or, by hand —
#   powershell -NoProfile -ExecutionPolicy Bypass -File ".\install.ps1"
# bypasses the execution policy for that one process and changes nothing else.
#
# Prefers building from source when Go >= 1.22 is installed; otherwise uses
# a prebuilt dist\be-code-windows-amd64.exe. Re-run to upgrade.
# Undo with .\uninstall.cmd.
param(
    [switch]$NoSetup
)
$ErrorActionPreference = "Stop"
$SrcDir  = Split-Path -Parent $MyInvocation.MyCommand.Path
$InstallDir = Join-Path $env:LOCALAPPDATA "Programs\be-code"
$Target = Join-Path $InstallDir "be-code.exe"

function Say($m) { Write-Host $m }

# ---- obtain a binary --------------------------------------------------------
$tmp = Join-Path $env:TEMP ("be-code-build-" + [guid]::NewGuid().ToString("N") + ".exe")
$built = $false

$go = Get-Command go -ErrorAction SilentlyContinue
if ($go -and (Test-Path (Join-Path $SrcDir "go.mod"))) {
    $ver = (& go env GOVERSION) -replace '^go', ''
    $parts = $ver.Split('.')
    if ([int]$parts[0] -gt 1 -or ([int]$parts[0] -eq 1 -and [int]$parts[1] -ge 22)) {
        Say "building from source with go $ver..."
        Push-Location $SrcDir
        try {
            $ver = "dev"
            $m = Select-String -Path (Join-Path $SrcDir "build.mk") -Pattern '^VERSION\s*:=\s*(.+)$' -ErrorAction SilentlyContinue
            if ($m) { $ver = $m.Matches[0].Groups[1].Value.Trim() }
            & go build -ldflags "-s -w -X github.com/brown-enterprises/be-code/cmd.Version=$ver" -o $tmp .
            if ($LASTEXITCODE -eq 0) { $built = $true } else { Say "warning: build failed; trying prebuilt binary" }
        } finally { Pop-Location }
    }
}

if (-not $built) {
    $prebuilt = Join-Path $SrcDir "dist\be-code-windows-amd64.exe"
    if (Test-Path $prebuilt) {
        Copy-Item $prebuilt $tmp -Force
        Say "using prebuilt binary: dist\be-code-windows-amd64.exe"
        $built = $true
    }
}
if (-not $built) {
    throw "No Go >=1.22 toolchain and no prebuilt dist\be-code-windows-amd64.exe. Install Go (https://go.dev/dl) or build 'make release' elsewhere."
}

& $tmp --help *> $null
if ($LASTEXITCODE -ne 0) { throw "binary failed a smoke test on this machine" }

# ---- install ----------------------------------------------------------------
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
Copy-Item $tmp $Target -Force
Remove-Item $tmp -Force
Say "installed $Target"

# ---- user PATH --------------------------------------------------------------
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if (($userPath -split ';') -notcontains $InstallDir) {
    [Environment]::SetEnvironmentVariable("Path", "$userPath;$InstallDir", "User")
    $env:Path = "$env:Path;$InstallDir"
    Say "added $InstallDir to your user PATH (new terminals pick it up automatically)"
}

# ---- first-run setup ---------------------------------------------------------
$cfg = Join-Path $env:USERPROFILE ".be-code\config.json"
if (-not $NoSetup -and -not (Test-Path $cfg)) {
    Say "running first-run setup (Ctrl+C to skip; re-run later with 'be-code setup')"
    & $Target setup
} else {
    Say "next steps:"
    Say "  be-code setup    # probe local backends and pick a model"
    Say "  be-code doctor   # check backend health"
    Say "  be-code          # start the TUI in a project directory"
}
Say "uninstall any time with: .\uninstall.cmd"
