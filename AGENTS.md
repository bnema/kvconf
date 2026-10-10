# kvconf

Stdlib-only Go library: `key = value` parser, struct decoder, native file
watchers and live reload. One package, no dependencies, ever.

## Layout

- `parse.go`: `Parse`, `ParseBytes`, `Options`, `Warning`.
- `decode.go`: struct tags, cached schema, `Decode`, `Unmarshal`.
- `marshal.go`: `Marshal`, `Quote`.
- `write.go`: `Set`, `SetFile`, `WriteFile`.
- `file.go`: `ReadFile`, `DefaultPath`, `Path`.
- `watch.go`: OS-independent watch loop (targets, symlink chain, stamps,
  debounce). Backends: `watch_linux.go` (inotify), `watch_kqueue*.go`
  (macOS/BSD), `watch_windows.go`, `watch_poll.go`, `watch_other.go`.
- `live.go`: `Load`, `Open`, `Live`.

## Rules

- Standard library only; `syscall` for the OS APIs, not `x/sys`.
- Warnings and errors never contain config values.
- Keep allocations out of hot paths; check with
  `go test -bench . -benchmem` and pprof before and after a change.
- Tests are table driven. Watch tests run on every backend.

## Checks

```sh
go vet ./... && go test -race ./...
golangci-lint run ./...
for os in darwin windows freebsd; do GOOS=$os go vet ./...; done
go test -run XXX -fuzz FuzzParse -fuzztime 30s .
```

CI (GitHub mirror) runs the tests on Linux, macOS and Windows.
