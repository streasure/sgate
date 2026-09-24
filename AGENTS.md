# AI Engineering Rules

## Logging

- All application logging MUST use `github.com/streasure/util/tlog`.
- Do NOT use `fmt.Print`, `fmt.Printf`, `fmt.Println`, `fmt.Fprintf`, `log.Print`, `log.Printf`, or `log.Println` for logging, diagnostics, debug output, or error reporting.
- Use `tlog.Debug`, `tlog.Info`, `tlog.Warn`, or `tlog.Error` with a `context.Context` and printf-style format arguments.
- `fmt` is permitted only for non-logging operations such as string formatting, serialization helpers, and error construction when no log output is produced.

## Architecture

- Package dependency direction is strictly one-way: `gateway → backend → connection`. Never introduce reverse imports.
- Shared protocol/frame helpers live in `internal/routes` (used by both gateway and backend).
- Consumer-defined interfaces: `backend.GatewayInterface` (implemented by `*gateway.Gateway`), `connection.LogicClientProvider` (implemented by `*backend.LogicClient`). Do not move these to producer packages.
- `component.NewContainer()` may only be called in `cmd/gateway/main.go`.
- Component constructors take no parameters and read config via `config.Get()`.
- Lifecycle order: Security(100) → Obs(200) → Traffic(300) → Cluster(400) → Gateway(1000).
- Global filter chain: initialize once with `types.InitFilterChain()` in main; components register via `types.GetFilterChain().AddFilter(...)`.
- Components publish state through exported globals in `internal/component/resources.go`; Gateway reads via getters.
- New code must pass `gofmt`, `go build ./...`, `go vet ./...`, and `go test ./... -count=1`.

## Benchmarks

- Bench configs MUST set `loginValidation.enabled: false` (login validation fully off).
- Benchmarks MUST NOT call the login HTTP API (no `/api/v1/login`); use only LoginGate (cmd 1000001).
- A successful bench must show no auth failures: no 401 / authfail / login-key rejects.
- Do not enable loginserver token validation during throughput benches.
- Never gate login solely on non-empty `loginKey` (empty key would bypass). Use `loginValidation.enabled`.
