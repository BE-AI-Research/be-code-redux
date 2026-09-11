# BE-Code uninstaller — Windows (PowerShell).
#
#   .\uninstall.ps1            remove the binary and PATH entry;
#                              KEEPS %USERPROFILE%\.be-code (config, sessions)
#   .\uninstall.ps1 -Purge     also delete %USERPROFILE%\.be-code (asks first)
#   .\uninstall.ps1 -Yes       don't ask for confirmation
param(
    [switch]$Purge,
    [switch]$Yes
)
$ErrorActionPreference = "Stop"
$InstallDir = Join-Path $env:LOCALAPPDATA "Programs\be-code"
$DataDir = Join-Path $env:USERPROFILE ".be-code"

function Say($m) { Write-Host $m }
function Confirm-Step($q) {
    if ($Yes) { return $true }
    return (Read-Host "$q [y/N]") -match '^(y|yes)$'
}

if (Test-Path $InstallDir) {
    if (Confirm-Step "remove $InstallDir?") {
        Remove-Item $InstallDir -Recurse -Force
        Say "removed $InstallDir"
    }
} else {
    Say "no installed be-code found in $InstallDir (already uninstalled?)"
}

# PATH entry
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
$parts = $userPath -split ';' | Where-Object { $_ -and $_ -ne $InstallDir }
if (($userPath -split ';') -contains $InstallDir) {
    [Environment]::SetEnvironmentVariable("Path", ($parts -join ';'), "User")
    Say "removed $InstallDir from your user PATH"
}

if ($Purge) {
    if (Test-Path $DataDir) {
        $count = (Get-ChildItem $DataDir -Recurse -File -ErrorAction SilentlyContinue).Count
        Say ""
        Say "-Purge will delete $DataDir ($count files): config, sessions, history, checkpoints, custom commands"
        if (Confirm-Step "permanently delete $DataDir?") {
            Remove-Item $DataDir -Recurse -Force
            Say "deleted $DataDir"
        } else {
            Say "kept $DataDir"
        }
    } else {
        Say "no $DataDir to purge"
    }
} elseif (Test-Path $DataDir) {
    Say "kept $DataDir (config/sessions/history) — remove with: .\uninstall.ps1 -Purge"
}

Say "BE-Code uninstall complete."
