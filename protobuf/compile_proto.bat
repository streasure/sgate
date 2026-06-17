@echo off

set PROTOC_PATH=..\protoc\protoc.exe
set PROTOC_GEN_GO=..\protoc\protoc-gen-go.exe
set PROTOC_GEN_GO_GRPC=..\protoc\protoc-gen-go-grpc.exe
set GO_OPT=Mmessage.proto=github.com/streasure/sgate/protobuf,Mgame.proto=github.com/streasure/sgate/protobuf,Muser.proto=github.com/streasure/sgate/protobuf,Mgateway.proto=github.com/streasure/sgate/protobuf

if not exist "%PROTOC_PATH%" (
    echo Error: protoc not found at %PROTOC_PATH%
    exit /b 1
)

"%PROTOC_PATH%" --go_out=paths=source_relative:. --go_opt=%GO_OPT% --go-grpc_out=paths=source_relative:. --go-grpc_opt=%GO_OPT% --plugin=protoc-gen-go=%PROTOC_GEN_GO% --plugin=protoc-gen-go-grpc=%PROTOC_GEN_GO_GRPC% gateway.proto
if %errorlevel% neq 0 (echo Error: gateway.proto failed & exit /b 1)

"%PROTOC_PATH%" --go_out=paths=source_relative:. --go_opt=%GO_OPT% --go-grpc_out=paths=source_relative:. --go-grpc_opt=%GO_OPT% --plugin=protoc-gen-go=%PROTOC_GEN_GO% --plugin=protoc-gen-go-grpc=%PROTOC_GEN_GO_GRPC% message.proto
if %errorlevel% neq 0 (echo Error: message.proto failed & exit /b 1)

"%PROTOC_PATH%" --go_out=paths=source_relative:. --go_opt=%GO_OPT% --plugin=protoc-gen-go=%PROTOC_GEN_GO% game.proto user.proto
if %errorlevel% neq 0 (echo Error: game.proto/user.proto failed & exit /b 1)

echo All proto files compiled successfully!
