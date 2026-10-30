@echo off
setlocal
cd /d "%~dp0"

set "PHANTHY_EXE=%CD%\bin\server.exe"
set "PHANTHY_PID_FILE=%CD%\phanthy.pid"
if not exist "%PHANTHY_PID_FILE%" (
  echo phanthywork2api is not running: no PID file.
  exit /b 1
)

set /p PHANTHY_PID=<"%PHANTHY_PID_FILE%"
powershell -NoProfile -Command "try { $p=Get-Process -Id ([int]$env:PHANTHY_PID) -ErrorAction Stop; if ([IO.Path]::GetFullPath($p.Path) -eq [IO.Path]::GetFullPath($env:PHANTHY_EXE)) { exit 0 } } catch {}; exit 1"
if errorlevel 1 (
  echo phanthywork2api is not running: stale PID file.
  exit /b 1
)

echo phanthywork2api is running. PID=%PHANTHY_PID%
curl.exe --fail-with-body --silent --show-error --max-time 5 "http://127.0.0.1:7866/healthz"
echo.
endlocal
