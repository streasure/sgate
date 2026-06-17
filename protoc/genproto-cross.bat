@echo off

set CUR_PATH=%~dp0
set INPUT_PATH=..\..\apicross
set OUTPUT_PATH=..\..\apicross

if not exist "%CUR_PATH%protoc.exe" (
    if not exist "%CUR_PATH%protoc" (
        echo Error: protoc not found in %CUR_PATH%
        exit /b 1
    )
    set PROTOC_CMD=%CUR_PATH%protoc
) else (
    set PROTOC_CMD=%CUR_PATH%protoc.exe
)

"%PROTOC_CMD%" -I %INPUT_PATH% ^
    --plugin=protoc-gen-go-grpc=%CUR_PATH%protoc-gen-go-grpc.exe ^
    --plugin=protoc-gen-go=%CUR_PATH%protoc-gen-go.exe  ^
    --go_out=%OUTPUT_PATH%\ --go-grpc_out=%OUTPUT_PATH% crossarena.proto

if %errorlevel% neq 0 (echo Error: crossarena.proto failed & exit /b 1)
echo SUCCESS