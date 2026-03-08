@echo off
setlocal
cd /d "%~dp0"
echo Running launcher: %~f0
echo Working directory: %cd%
echo.

if not exist spank.exe (
  echo spank.exe not found. Building now...
  if exist "C:\Program Files\Go\bin\go.exe" (
    "C:\Program Files\Go\bin\go.exe" build -o spank.exe .
  ) else (
    go build -o spank.exe .
  )
)

if not exist spank.exe (
  echo Build failed. Please ensure Go is installed and try again.
  pause
  exit /b 1
)

echo Starting spank in manual trigger mode...
echo Press Enter to trigger sounds. Type q then press Enter to quit.
spank.exe --trigger enter --test-audio

endlocal
