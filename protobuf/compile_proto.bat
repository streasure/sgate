@echo off

REM 编译Protocol Buffers文件
REM 使用protoc文件夹下的protoc可执行文件编译message.proto文件

REM 定义protoc路径
set PROTOC_PATH=../../protoc/protoc
set PROTOC_GEN_GO=../../protoc/protoc-gen-go
set PROTOC_GEN_GO_GRPC=../../protoc/protoc-gen-go-grpc

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

REM 检查protoc-gen-go-grpc是否存在
if not exist "%PROTOC_GEN_GO_GRPC%" (
    echo Error: protoc-gen-go-grpc is not found at %PROTOC_GEN_GO_GRPC%
    pause
    exit /b 1
)

REM 直接使用完整路径调用protoc和插件，不依赖系统环境变量
"%PROTOC_PATH%" --go_out=paths=source_relative:. --go_opt=Mmessage.proto=github.com/streasure/sgate/protobuf --go-grpc_out=paths=source_relative:. --go-grpc_opt=Mmessage.proto=github.com/streasure/sgate/protobuf gateway.proto

"%PROTOC_PATH%" --go_out=paths=source_relative:. --go_opt=Mmessage.proto=github.com/streasure/sgate/protobuf message.proto

if %errorlevel% eq 0 (
    echo Protocol Buffers compilation successful!
) else (
    echo Error: Protocol Buffers compilation failed.
    pause
    exit /b 1
)

pause