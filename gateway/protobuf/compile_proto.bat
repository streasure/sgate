@echo off

REM 编译Protocol Buffers文件
REM 使用protoc文件夹下的protoc可执行文件编译message.proto文件

REM 定义protoc路径
set PROTOC_PATH=../../protoc/protoc
set PROTOC_GEN_GO=../../protoc/protoc-gen-go

REM 检查protoc是否存在
if not exist "%PROTOC_PATH%" (
    echo Error: protoc is not found at %PROTOC_PATH%
    pause
    exit /b 1
)

REM 检查protoc-gen-go是否存在
if not exist "%PROTOC_GEN_GO%" (
    echo Error: protoc-gen-go is not found at %PROTOC_GEN_GO%
    pause
    exit /b 1
)

REM 设置PATH环境变量，确保protoc-gen-go可以被找到
set PATH=%PATH%;../../protoc

REM 编译message.proto
"%PROTOC_PATH%" --go_out=paths=source_relative:. message.proto

if %errorlevel% eq 0 (
    echo Protocol Buffers compilation successful!
) else (
    echo Error: Protocol Buffers compilation failed.
    pause
    exit /b 1
)

pause