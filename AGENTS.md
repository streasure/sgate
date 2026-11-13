# AI Engineering Rules

## Logging

- All application logging MUST use `github.com/streasure/util/tlog`.
- Do NOT use `fmt.Print`, `fmt.Printf`, `fmt.Println`, `fmt.Fprintf`, `log.Print`, `log.Printf`, or `log.Println` for logging, diagnostics, debug output, or error reporting.
- Use `tlog.Debug`, `tlog.Info`, `tlog.Warn`, or `tlog.Error` with a `context.Context` and printf-style format arguments.
- `fmt` is permitted only for non-logging operations such as string formatting, serialization helpers, and error construction when no log output is produced.

## Architecture

- Package dependency direction is strictly one-way: `gateway → backend → connection`. Never introduce reverse imports.
- Frame codec helpers are owned by the wire repo: `github.com/streasure/protocol/gateway` (`frame.go`: DecodeClientMessage/PutClientMessage/MarshalClientMessage/NewErrorFrame/IsUDPCmd/ExtractMessageFrame). Business packages must never re-implement frame parsing; `internal/routes` has been removed.
- Consumer-defined interfaces: `backend.GatewayInterface` (implemented by `*gateway.Gateway`), `connection.LogicClientProvider` (implemented by `*backend.LogicClient`). Do not move these to producer packages.
- `component.NewContainer()` may only be called in `cmd/gateway/main.go`.
- Component constructors take `*config.Config` explicitly (the composition root `cmd/gateway/main.go` passes the result of `config.Load`); there is no package-level config state (`config.Get`/`config.Path` must not come back).
- Lifecycle order: Security(100) → Obs(200) → Traffic(300) → Cluster(400) → Gateway(1000).
- Runtime resources are injected: the composition root creates one `*component.Resources` (filter chain `FC` included) via `component.NewResources()` and passes it to every component constructor and `NewGateway`; components write `res.XXX` / `res.FC.AddFilter` in `Init` (Order-sequenced), Gateway reads `g.res` in its `Init`. No package-level resource globals, no exported getters, no `types.InitFilterChain`/`types.GetFilterChain` (only init-time `types.RegisterFilter` SPI registration remains).
- New code must pass `gofmt`, `go build ./...`, `go vet ./...`, and `go test ./... -count=1`.

## Panic Recovery

- Do NOT hand-write `if r := recover()` templates. Use `github.com/streasure/util/panicutil`:
  - `panicutil.Go(ctx, tag, fn)` — one-shot async worker; panic logged with stack, no restart.
  - `panicutil.GoLoop(ctx, tag, loopFn)` — long-running loop; panic logs + restarts after 1s backoff; normal `return` (stop signal) ends the loop. Loop must own its stop condition (`stopChan` etc.); close/cleanup channels only on the normal-return path (restart must not double-close).
  - `panicutil.Recover(ctx, tag)` — `defer` in synchronous callbacks (gnet/gRPC handlers) that only need logging.
  - `panicutil.Safe(ctx, tag, fn)` — per-iteration guard inside a loop (one message / one check); panic does not kill the outer loop.
- Tag convention: `package.component` (e.g. `gateway.wsHeartbeatChecker`, `backend.receiveMessages`); may append static-ish context (`backend.sendBatch shard=%d count=%d`).
- Hand-written recover is allowed ONLY when panic must affect a return value beyond logging (e.g. gnet `action = gnet.Close`, SendMessage converting send-on-closed-channel panic into `error`).

## Benchmarks

- Bench package layout is intentional: each scenario is a standalone `main` under `bench/*` for isolated binaries and fair profiling. Do NOT merge bench mains into a single multi-command binary.
- Bench configs MUST set `loginValidation.enabled: false` (login validation fully off).
- Benchmarks MUST NOT call the login HTTP API (no `/api/v1/login`); use only LoginGate (cmd 1000001).
- A successful bench must show no auth failures: no 401 / authfail / login-key rejects.
- Do not enable loginserver token validation during throughput benches.
- Never gate login solely on non-empty `loginKey` (empty key would bypass). Use `loginValidation.enabled`.
