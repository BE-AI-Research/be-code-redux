#Requires -Version 5.1
<#
.SYNOPSIS
    Builds BECode.VisualStudio.csproj on Windows and prints the resulting
    .vsix path. Everything up to packaging is proven by `dotnet build` on
    Linux (see the repo's CI and
    docs/superpowers/specs/2026-09-20-visual-studio-host-design.md); this
    script exists because the VSIX container itself (Microsoft.VSSDK.BuildTools'
    MSBuild targets) can only be produced by MSBuild.exe on a machine with
    Visual Studio's "Visual Studio extension development" workload
    installed — there is no `dotnet build` equivalent for that step.

.DESCRIPTION
    1. Locates MSBuild via vswhere.exe (bundled with every Visual Studio
       2017+ installation, including Build Tools).
    2. Restores NuGet packages for the VSIX project only.
    3. Builds BECode.VisualStudio.csproj (NOT the whole solution — the
       solution also carries BECode.Bridge.FakeHost and the net8.0 test
       project, and MSBuild from a VS 2022 17.6 install carries the .NET 7
       SDK and fails those with NETSDK1045 before any .vsix is produced).
    4. Finds the built .vsix (recursively — an SDK-style project outputs to
       bin\<Configuration>\net472\, not bin\<Configuration>\ directly) and
       prints its path.
    5. Opens the .vsix as a zip and fails loudly if BECode.Bridge.dll is not
       inside it — the one thing this script exists to catch before the
       owner finds out the hard way inside devenv.exe.

    A missing vswhere, a missing MSBuild, or a build failure that looks
    like the VSSDK targets are absent all produce a readable, specific
    error rather than a raw MSBuild stack dump.
#>
[CmdletBinding()]
param(
    [string]$Configuration = "Release"
)

# With $ErrorActionPreference = "Stop", Write-Error is a
# TERMINATING error — it throws immediately, so any "exit $code" written
# after it never runs and the script's real exit code is lost (PowerShell's
# own default for an uncaught terminating error, not whatever the caller
# passed). Every error path below prints with Write-Host -ForegroundColor Red
# (or [Console]::Error.WriteLine for a pre-formatted block) and THEN calls
# exit explicitly, so the exit code the caller sees is the one this script
# chose, not an accident of how Write-Error unwinds.
$ErrorActionPreference = "Stop"

function Write-ErrorBlock {
    param([string]$Text)
    [Console]::Error.WriteLine($Text)
}

# $PSScriptRoot (this script's own directory), not
# Split-Path -Parent $MyInvocation.MyCommand.Path — the latter is empty when
# this script is dot-sourced rather than invoked directly.
$repoRoot = $PSScriptRoot
$vsixProject = Join-Path $repoRoot "src\BECode.VisualStudio\BECode.VisualStudio.csproj"

if (-not (Test-Path $vsixProject)) {
    Write-ErrorBlock "build.ps1: could not find $vsixProject — run this script from a checkout of the repo, not a copied-out copy of just this file."
    exit 1
}

function Find-VsWhere {
    $candidate = Join-Path ${env:ProgramFiles(x86)} "Microsoft Visual Studio\Installer\vswhere.exe"
    if (Test-Path $candidate) {
        return $candidate
    }

    $onPath = Get-Command "vswhere.exe" -ErrorAction SilentlyContinue
    if ($onPath) {
        return $onPath.Source
    }

    return $null
}

function Find-MSBuild {
    $vswhere = Find-VsWhere
    if (-not $vswhere) {
        Write-ErrorBlock @"
build.ps1: could not find vswhere.exe (normally at
"${env:ProgramFiles(x86)}\Microsoft Visual Studio\Installer\vswhere.exe").
This usually means Visual Studio itself is not installed on this machine —
install Visual Studio 2022 (17.6+) or 2026 with the "Visual Studio
extension development" workload, or install the standalone Build Tools
with that workload, then re-run this script.
"@
        exit 1
    }

    # Ask vswhere to FIND MSBuild.exe itself under the
    # installation's own MSBuild tree, rather than hard-coding
    # "MSBuild\Current\Bin" — VS 2026's own layout there is unverified from
    # this Linux checkout, and -find is exactly what vswhere exists for.
    $found = & $vswhere -latest -prerelease -products * `
        -requires Microsoft.VisualStudio.Workload.VisualStudioExtension `
        -find "MSBuild\**\Bin\MSBuild.exe"

    if (-not $found) {
        Write-ErrorBlock @"
build.ps1: vswhere found no Visual Studio installation with the "Visual
Studio extension development" workload (or no MSBuild.exe under it). Open
the Visual Studio Installer, choose "Modify" on your Visual Studio 2022 or
2026 install, and check "Visual Studio extension development" under
Workloads, then re-run this script.
"@
        exit 1
    }

    # -find can print more than one match; the first is vswhere's own
    # preference ordering (newest/most specific installation first).
    $msbuild = @($found)[0]
    if (-not (Test-Path $msbuild)) {
        Write-ErrorBlock "build.ps1: vswhere reported MSBuild at $msbuild but it was not there — this Visual Studio installation may be incomplete."
        exit 1
    }

    return $msbuild
}

$msbuildPath = Find-MSBuild
Write-Host "build.ps1: using MSBuild at $msbuildPath"

Write-Host "build.ps1: restoring $vsixProject ..."
& $msbuildPath $vsixProject "/t:Restore" "/p:Configuration=$Configuration" "/nologo" "/verbosity:minimal"
if ($LASTEXITCODE -ne 0) {
    Write-ErrorBlock "build.ps1: NuGet restore failed (exit code $LASTEXITCODE) — see the MSBuild output above."
    exit $LASTEXITCODE
}

Write-Host "build.ps1: building $vsixProject ($Configuration) ..."
& $msbuildPath $vsixProject "/p:Configuration=$Configuration" "/nologo" "/verbosity:minimal"
$buildExitCode = $LASTEXITCODE

if ($buildExitCode -ne 0) {
    Write-ErrorBlock @"
build.ps1: build failed (exit code $buildExitCode). If the errors above
mention GeneratePkgDefFile, VSSDK, VsSDK.targets or a missing
Microsoft.VsSDK.Common.targets, the "Visual Studio extension development"
workload is missing or incomplete on this machine — see the error above
for how to install it. Otherwise, see the MSBuild output above for the
actual compile error.
"@
    exit $buildExitCode
}

# An SDK-style project outputs to
# bin\<Configuration>\net472\, not bin\<Configuration>\ directly — search
# recursively and take the newest .vsix in case a stale one from an older
# TargetFramework layout is still sitting there.
$vsixDir = Join-Path (Split-Path -Parent $vsixProject) "bin\$Configuration"
$vsix = Get-ChildItem -Path $vsixDir -Filter "*.vsix" -Recurse -ErrorAction SilentlyContinue |
    Sort-Object LastWriteTime -Descending |
    Select-Object -First 1

if (-not $vsix) {
    Write-ErrorBlock "build.ps1: build reported success but no .vsix was found anywhere under $vsixDir — check that CreateVsixContainer is enabled (it is, under the Windows-only condition in BECode.VisualStudio.csproj) and that the workload above is actually installed."
    exit 1
}

# The .vsix is a zip file. List its contents and fail
# loudly if BECode.Bridge.dll is not inside — a silent, empty-handed
# "success" here is exactly the failure mode this script exists to catch
# before the owner installs it into devenv.exe and finds out the hard way.
Add-Type -AssemblyName System.IO.Compression.FileSystem
$zip = [IO.Compression.ZipFile]::OpenRead($vsix.FullName)
try {
    $entryNames = $zip.Entries | ForEach-Object { $_.FullName }
} finally {
    $zip.Dispose()
}

Write-Host ""
Write-Host "build.ps1: $($vsix.Name) contains:"
$entryNames | Sort-Object | ForEach-Object { Write-Host "  $_" }

$bridgeEntry = $entryNames | Where-Object { $_ -eq "BECode.Bridge.dll" }
if (-not $bridgeEntry) {
    Write-ErrorBlock "build.ps1: $($vsix.Name) was built but does NOT contain BECode.Bridge.dll — the bridge would fail to load inside devenv.exe. See host design section 1.1/1.2 and the csproj's ProjectReference to BECode.Bridge."
    exit 1
}

# Both BE-Code extensions package to a ".vsix", so the file is named for its
# editor and put beside the VS Code one in the repository's dist\ folder. The
# name is set here from the manifest's own version rather than trusted from
# the build, so it is right even if the project's container name is not.
[xml]$manifest = Get-Content (Join-Path (Split-Path -Parent $vsixProject) "source.extension.vsixmanifest")
$extensionVersion = $manifest.PackageManifest.Metadata.Identity.Version
$distDir = Join-Path (Split-Path -Parent $repoRoot) "dist"
New-Item -ItemType Directory -Force -Path $distDir | Out-Null
$named = Join-Path $distDir "be-code-visualstudio-$extensionVersion.vsix"
Copy-Item -Path $vsix.FullName -Destination $named -Force

Write-Host ""
Write-Host "build.ps1: built $named"
Write-Host "           (BE-Code for Visual Studio $extensionVersion - the VS Code extension is be-code-vscode-<version>.vsix)"
Write-Output $named
