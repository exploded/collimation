@echo off
rem Starts the collimation station and opens it in the browser.
rem Switch to the folder this file is in (%~dp0 = this file's drive and folder;
rem /d also changes drive), so it works from a shortcut or another drive.
cd /d "%~dp0"
if not exist collimation.exe (
  echo collimation.exe not found. Run "go build" in this folder first.
  pause
  exit /b 1
)
collimation.exe
