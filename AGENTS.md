# AI Engineering Rules

## Logging

- All application logging MUST use `github.com/streasure/util/tlog`.
- Do NOT use `fmt.Print`, `fmt.Printf`, `fmt.Println`, `fmt.Fprintf`, `log.Print`, `log.Printf`, or `log.Println` for logging, diagnostics, debug output, or error reporting.
- Use `tlog.Debug`, `tlog.Info`, `tlog.Warn`, or `tlog.Error` with a `context.Context` and printf-style format arguments.
- `fmt` is permitted only for non-logging operations such as string formatting, serialization helpers, and error construction when no log output is produced.
