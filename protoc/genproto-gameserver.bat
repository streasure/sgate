@echo off

:: path
set CUR_PATH=%~dp0
set ROOT_PATH=
set INPUT_PATH=..\..\api\rpc
set OUTPUT_PATH=..\..\api\rpc

protoc.exe -I %INPUT_PATH% ^
    --plugin=protoc-gen-go-grpc=%CUR_PATH%\protoc-gen-go-grpc.exe ^
    --plugin=protoc-gen-go=%CUR_PATH%\protoc-gen-go.exe  ^
    --go_out=%OUTPUT_PATH%\ --go-grpc_out=%OUTPUT_PATH% gameserver.proto

cd /d %CUR_PATH%
@echo "SUCCESS"
pause