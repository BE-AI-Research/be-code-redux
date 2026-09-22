@echo off
rem BE-Code: Visual Studio extension build launcher.
rem
rem Windows refuses to run unsigned PowerShell scripts by default ("running
rem scripts is disabled on this system"), and a script unpacked from a
rem downloaded zip is blocked even where local scripts are allowed. This runs
rem build.ps1 with the execution policy bypassed for this one process only:
rem the machine's policy is not changed, and no administrator rights are needed.
rem Arguments are passed through, e.g.  build.cmd 
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0build.ps1" %*
exit /b %ERRORLEVEL%
