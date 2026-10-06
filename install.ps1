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
# In a checkout it prefers building from source when Go >= 1.25 is installed,
# otherwise a prebuilt dist\be-code-windows-amd64.exe. With no checkout — the
# one-line install, which pipes this file into PowerShell:
#   irm https://raw.githubusercontent.com/BE-AI-Research/be-code-redux/main/install.ps1 | iex
# — it downloads the current release's binary from GitHub and checks it
# against the release's SHA256SUMS before installing. BE_CODE_VERSION pins a
# release; BE_CODE_NO_SETUP=1 skips the setup wizard (iex takes no -NoSetup).
# Re-run to upgrade. Undo with .\uninstall.cmd (or uninstall.ps1 the same way).
param(
    [switch]$NoSetup
)
$ErrorActionPreference = "Stop"
$Repo = "BE-AI-Research/be-code-redux"
# Piped into iex there is no script file, so no checkout beside it.
$SrcDir = $null
if ($MyInvocation.MyCommand.Path) { $SrcDir = Split-Path -Parent $MyInvocation.MyCommand.Path }
$Local = $SrcDir -and (Test-Path (Join-Path $SrcDir "go.mod")) -and (Test-Path (Join-Path $SrcDir "build.mk"))
if ($env:BE_CODE_NO_SETUP -eq "1") { $NoSetup = $true }
$InstallDir = Join-Path $env:LOCALAPPDATA "Programs\be-code"
$Target = Join-Path $InstallDir "be-code.exe"

function Say($m) { Write-Host $m }

# ---- obtain a binary --------------------------------------------------------
$tmp = Join-Path $env:TEMP ("be-code-build-" + [guid]::NewGuid().ToString("N") + ".exe")
$built = $false

if (-not $Local) {
    # Windows PowerShell 5.1 may not offer TLS 1.2 by default; GitHub needs it.
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    $ver = $env:BE_CODE_VERSION
    if (-not $ver) {
        $mk = Invoke-RestMethod "https://raw.githubusercontent.com/$Repo/main/build.mk"
        $m = [regex]::Match($mk, '(?m)^VERSION\s*:=\s*(\S+)')
        if (-not $m.Success) { throw "no VERSION line in https://raw.githubusercontent.com/$Repo/main/build.mk" }
        $ver = $m.Groups[1].Value
    }
    $ver = $ver.TrimStart('v')
    $asset = "be-code-windows-amd64.exe"
    $base = "https://github.com/$Repo/releases/download/v$ver"
    Say "fetching be-code $ver for windows/amd64 from $Repo..."
    Invoke-WebRequest -UseBasicParsing -Uri "$base/$asset" -OutFile $tmp
    $sums = (Invoke-WebRequest -UseBasicParsing -Uri "$base/SHA256SUMS").Content
    if ($sums -is [byte[]]) { $sums = [Text.Encoding]::ASCII.GetString($sums) }
    $line = ($sums -split "`n") | ForEach-Object { $_.Trim() } | Where-Object { $_ -and ($_ -split '\s+')[-1].TrimStart('*') -eq $asset } | Select-Object -First 1
    if (-not $line) { Remove-Item $tmp -Force; throw "v$ver has no SHA256SUMS line for $asset; not installed" }
    $want = ($line -split '\s+')[0].ToLower()
    $got = (Get-FileHash -Algorithm SHA256 $tmp).Hash.ToLower()
    if ($got -ne $want) { Remove-Item $tmp -Force; throw "the downloaded $asset does not match v$ver's SHA256SUMS; not installed" }
    Say "downloaded be-code $ver (checksum verified)"
    $built = $true
}

$go = Get-Command go -ErrorAction SilentlyContinue
if (-not $built -and $go -and $Local) {
    $ver = (& go env GOVERSION) -replace '^go', ''
    $parts = $ver.Split('.')
    if ([int]$parts[0] -gt 1 -or ([int]$parts[0] -eq 1 -and [int]$parts[1] -ge 25)) {
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

if (-not $built -and $Local) {
    $prebuilt = Join-Path $SrcDir "dist\be-code-windows-amd64.exe"
    if (Test-Path $prebuilt) {
        Copy-Item $prebuilt $tmp -Force
        Say "using prebuilt binary: dist\be-code-windows-amd64.exe"
        $built = $true
    }
}
if (-not $built) {
    throw "No Go >=1.25 toolchain and no prebuilt dist\be-code-windows-amd64.exe. Install Go (https://go.dev/dl), build 'make release' elsewhere, or install the published release with: irm https://raw.githubusercontent.com/$Repo/main/install.ps1 | iex"
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
if ($Local) {
    Say "uninstall any time with: .\uninstall.cmd"
} else {
    Say "uninstall any time with: irm https://raw.githubusercontent.com/$Repo/main/uninstall.ps1 | iex"
}
