@echo off
rem Starts the collimation station and opens it in the browser.
rem Build first with "go build" in this folder if collimation.exe is missing or out of date.
cd /d "%~dp0"
if not exist collimation.exe (
  echo collimation.exe not found - building it now...
  go build || (pause & exit /b 1)
)
collimation.exe
