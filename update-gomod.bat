@echo off
REM ============================================================
REM update-gomod.bat - run "go mod tidy" with sumdb check skipped
REM for github.com/streasure modules (they are not in the public
REM checksum database, so verification would fail with 404).
REM
REM Usage:
REM   1. Edit go.mod manually and set the desired version(s).
REM   2. Run update-gomod.bat (from anywhere; the script switches
REM      to its own folder first).
REM
REM Notes:
REM   GONOSUMDB is set only inside this script process; the global
REM   Go config is NOT modified. Once hashes are written into
REM   go.sum, later builds use the local record and never query
REM   the sumdb again.
REM   Do NOT use GOPRIVATE/GONOPROXY instead: that would bypass
REM   goproxy.cn and connect to github.com directly (unreachable
REM   on this machine).
REM
REM Encoding: keep this file pure ASCII. cmd.exe parses batch
REM files with the active console codepage; multibyte characters
REM (Chinese) break the parser.
REM ============================================================

setlocal
cd /d "%~dp0"

set "GONOSUMDB=yofijoy.com,github.com/streasure"

echo go mod tidy ...
go mod tidy
if errorlevel 1 goto fail

echo.
echo ==== OK: go mod tidy finished ====
endlocal
exit /b 0

:fail
echo.
echo ==== FAILED: see errors above ====
endlocal
exit /b 1
