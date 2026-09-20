#Requires -Version 5.1
<#
.SYNOPSIS
    Builds BECode.VisualStudio.sln on Windows and prints the resulting
    .vsix path. This script is Task 6's Windows half: everything up to
    packaging is proven by `dotnet build` on Linux (see the repo's CI and
    docs/superpowers/specs/2026-09-20-visual-studio-host-design.md); this
    script exists because the VSIX container itself (Microsoft.VSSDK.BuildTools'
    MSBuild targets) can only be produced by MSBuild.exe on a machine with
    Visual Studio's "Visual Studio extension development" workload
    installed — there is no `dotnet build` equivalent for that step.

.DESCRIPTION
    1. Locates MSBuild via vswhere.exe (bundled with every Visual Studio
       2017+ installation, including Build Tools).
    2. Restores NuGet packages for the solution.
    3. Builds BECode.VisualStudio.sln in the Release configuration.
    4. Prints the path to the built .vsix.

    A missing vswhere, a missing MSBuild, or a build failure that looks
    like the VSSDK targets are absent all produce a readable, specific
    error rather than a raw MSBuild stack dump.
#>
[CmdletBinding()]
param(
    [string]$Configuration = "Release"
)

$ErrorActionPreference = "Stop"

$repoRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
$solution = Join-Path $repoRoot "BECode.VisualStudio.sln"
$vsixProject = Join-Path $repoRoot "src\BECode.VisualStudio\BECode.VisualStudio.csproj"

if (-not (Test-Path $solution)) {
    Write-Error "build.ps1: could not find $solution — run this script from a checkout of the repo, not a copied-out copy of just this file."
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
        Write-Error @"
build.ps1: could not find vswhere.exe (normally at
"${env:ProgramFiles(x86)}\Microsoft Visual Studio\Installer\vswhere.exe").
This usually means Visual Studio itself is not installed on this machine —
install Visual Studio 2022 (17.6+) or 2026 with the "Visual Studio
extension development" workload, or install the standalone Build Tools
with that workload, then re-run this script.
"@
        exit 1
    }

    $installPath = & $vswhere -latest -prerelease -products * `
        -requires Microsoft.VisualStudio.Workload.VisualStudioExtension `
        -property installationPath

    if (-not $installPath) {
        Write-Error @"
build.ps1: vswhere found a Visual Studio installation but none of them has
the "Visual Studio extension development" workload installed. Open the
Visual Studio Installer, choose "Modify" on your Visual Studio 2022 or 2026
install, and check "Visual Studio extension development" under Workloads,
then re-run this script.
"@
        exit 1
    }

    $msbuild = Join-Path $installPath "MSBuild\Current\Bin\MSBuild.exe"
    if (-not (Test-Path $msbuild)) {
        Write-Error "build.ps1: expected MSBuild.exe at $msbuild but it was not there — this Visual Studio installation may be incomplete."
        exit 1
    }

    return $msbuild
}

$msbuildPath = Find-MSBuild
Write-Host "build.ps1: using MSBuild at $msbuildPath"

Write-Host "build.ps1: restoring $solution ..."
& $msbuildPath $solution "/t:Restore" "/p:Configuration=$Configuration" "/nologo" "/verbosity:minimal"
if ($LASTEXITCODE -ne 0) {
    Write-Error "build.ps1: NuGet restore failed (exit code $LASTEXITCODE) — see the MSBuild output above."
    exit $LASTEXITCODE
}

Write-Host "build.ps1: building $solution ($Configuration) ..."
& $msbuildPath $solution "/p:Configuration=$Configuration" "/nologo" "/verbosity:minimal"
$buildExitCode = $LASTEXITCODE

if ($buildExitCode -ne 0) {
    Write-Error @"
build.ps1: build failed (exit code $buildExitCode). If the errors above
mention GeneratePkgDefFile, VSSDK, VsSDK.targets or a missing
Microsoft.VsSDK.Common.targets, the "Visual Studio extension development"
workload is missing or incomplete on this machine — see the error above
for how to install it. Otherwise, see the MSBuild output above for the
actual compile error.
"@
    exit $buildExitCode
}

$vsixDir = Join-Path (Split-Path -Parent $vsixProject) "bin\$Configuration"
$vsix = Get-ChildItem -Path $vsixDir -Filter "*.vsix" -ErrorAction SilentlyContinue |
    Sort-Object LastWriteTime -Descending |
    Select-Object -First 1

if (-not $vsix) {
    Write-Error "build.ps1: build reported success but no .vsix was found under $vsixDir — check that CreateVsixContainer is enabled (it is, under the Windows-only condition in BECode.VisualStudio.csproj) and that the workload above is actually installed."
    exit 1
}

Write-Host ""
Write-Host "build.ps1: built $($vsix.FullName)"
Write-Output $vsix.FullName
