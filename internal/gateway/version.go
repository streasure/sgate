package gateway

// Version 服务器对外/内部版本号（默认）。
const Version = "v1.0.1"

// BuildVersion 构建注入版本，通常通过 -ldflags 覆盖；未注入时回退到 Version。
var BuildVersion = Version
