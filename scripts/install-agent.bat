@echo off
REM Install rewardd-agent.msi (requires Administrator).
REM Usage:
REM   install-agent.bat rewardd-agent.msi http://192.168.88.70:8080 mytoken leo 1234
setlocal
if "%~5"=="" (
  echo Usage: %~nx0 MSI_PATH SERVER_URL TOKEN KIDUSER [PIN]
  echo Example: %~nx0 rewardd-agent.msi "http://192.168.88.70:8080" mytoken leo 1234
  exit /b 1
)

set MSI=%~1
set SERVER=%~2
set TOKEN=%~3
set KIDUSER=%~4
set PIN=%~5
set LOG=%TEMP%\rewardd-install.log

echo Installing from %MSI%
echo Log: %LOG%
echo.

msiexec /i "%MSI%" /qn /L*v "%LOG%" ^
  SERVER="%SERVER%" TOKEN="%TOKEN%" KIDUSER="%KIDUSER%" PIN="%PIN%"

if errorlevel 1 (
  echo Install failed. See %LOG%
  exit /b 1
)

echo Installed. Checking service...
sc query rewardd-agent-svc
echo.
echo Config: %ProgramData%\rewardd\agent.conf
exit /b 0
