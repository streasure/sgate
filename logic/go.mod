module github.com/streasure/sgate/logic

go 1.22.5

require (
	github.com/streasure/sgate v0.0.0-00010101000000-000000000000
	google.golang.org/grpc v1.64.0
)

require (
	github.com/streasure/treasure-slog v1.0.6 // indirect
	golang.org/x/net v0.22.0 // indirect
	golang.org/x/sys v0.30.0 // indirect
	golang.org/x/text v0.14.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20240318140521-94a12d6c2237 // indirect
	google.golang.org/protobuf v1.33.0 // indirect
	gopkg.in/natefinch/lumberjack.v2 v2.2.1 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// 直接引用根目录的模块
replace github.com/streasure/sgate => ..
