@echo off
setlocal EnableDelayedExpansion
cd /d "%~dp0"

if not exist "bin\server.exe" (
  echo Missing bin\server.exe. Build it first.
  exit /b 1
)
if not exist "config.json" (
  echo Missing config.json.
  exit /b 1
)

set "PHANTHY_ROOT=%CD%"
set "PHANTHY_EXE=%CD%\bin\server.exe"
set "PHANTHY_PID_FILE=%CD%\phanthy.pid"

if exist "%PHANTHY_PID_FILE%" (
  set /p PHANTHY_PID=<"%PHANTHY_PID_FILE%"
  powershell -NoProfile -Command "try { $p=Get-Process -Id ([int]$env:PHANTHY_PID) -ErrorAction Stop; if ([IO.Path]::GetFullPath($p.Path) -eq [IO.Path]::GetFullPath($env:PHANTHY_EXE)) { exit 0 } } catch {}; exit 1"
  if not errorlevel 1 (
    echo phanthywork2api is already running. PID=!PHANTHY_PID!
    exit /b 0
  )
  del /q "%PHANTHY_PID_FILE%" >nul 2>&1
)

if not exist "data" mkdir "data"
powershell -NoProfile -Command "$p=Start-Process -FilePath $env:PHANTHY_EXE -ArgumentList '-config','config.json' -WorkingDirectory $env:PHANTHY_ROOT -WindowStyle Hidden -RedirectStandardOutput (Join-Path $env:PHANTHY_ROOT 'data\server.out.log') -RedirectStandardError (Join-Path $env:PHANTHY_ROOT 'data\server.err.log') -PassThru; [IO.File]::WriteAllText($env:PHANTHY_PID_FILE, [string]$p.Id)"
if errorlevel 1 exit /b 1

set /p PHANTHY_PID=<"%PHANTHY_PID_FILE%"
echo phanthywork2api started on port 7866. PID=%PHANTHY_PID%
endlocal
