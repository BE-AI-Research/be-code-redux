@echo off
rem BE-Code: installer launcher.
rem
rem Windows refuses to run unsigned PowerShell scripts by default ("running
rem scripts is disabled on this system"), and a script unpacked from a
rem downloaded zip is blocked even where local scripts are allowed. This runs
rem install.ps1 with the execution policy bypassed for this one process only:
rem the machine's policy is not changed, and no administrator rights are needed.
rem Arguments are passed through, e.g.  install.cmd -NoSetup
rem
rem Downloaded on its own (the one-line install from cmd.exe):
rem   curl -fsSLo "%TEMP%\be-code-install.cmd" https://raw.githubusercontent.com/BE-AI-Research/be-code-redux/main/install.cmd && "%TEMP%\be-code-install.cmd"
rem there is no install.ps1 beside it, so it runs the published installer
rem from GitHub, which downloads the current release and checks its checksum.
rem No labels or ( ) blocks: GitHub serves this file with LF line endings,
rem and cmd.exe can misread a goto label in an LF-only batch file. No `&` on
rem an `if` line either: cmd.exe runs what follows `&` whether the condition
rem matched or not, so `if exist install.ps1 powershell ... & exit /b` exited
rem even when there was no install.ps1, which is the download-on-its-own
rem case, and the GitHub fallback below was never reached. Hence the flag:
rem each `if` governs exactly one command.
set BECODE_LOCAL=
if exist "%~dp0install.ps1" set BECODE_LOCAL=1
if defined BECODE_LOCAL powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0install.ps1" %*
if defined BECODE_LOCAL exit /b %ERRORLEVEL%
if /i "%~1"=="-NoSetup" set BE_CODE_NO_SETUP=1
powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "irm https://raw.githubusercontent.com/BE-AI-Research/be-code-redux/main/install.ps1 | iex"
exit /b %ERRORLEVEL%
