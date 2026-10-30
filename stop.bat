@echo off
setlocal
cd /d "%~dp0"

set "PHANTHY_EXE=%CD%\bin\server.exe"
set "PHANTHY_PID_FILE=%CD%\phanthy.pid"
if not exist "%PHANTHY_PID_FILE%" (
  echo phanthywork2api is not running: no PID file.
  exit /b 0
)

set /p PHANTHY_PID=<"%PHANTHY_PID_FILE%"
powershell -NoProfile -Command "try { $p=Get-Process -Id ([int]$env:PHANTHY_PID) -ErrorAction Stop; if ([IO.Path]::GetFullPath($p.Path) -eq [IO.Path]::GetFullPath($env:PHANTHY_EXE)) { exit 0 } } catch {}; exit 1"
if errorlevel 1 (
  echo Removed stale PID file; no process was stopped.
  del /q "%PHANTHY_PID_FILE%" >nul 2>&1
  exit /b 0
)

taskkill /PID %PHANTHY_PID% /T /F >nul 2>&1
if errorlevel 1 (
  echo Failed to stop phanthywork2api PID=%PHANTHY_PID%.
  exit /b 1
)
del /q "%PHANTHY_PID_FILE%" >nul 2>&1
echo phanthywork2api stopped.
endlocal
