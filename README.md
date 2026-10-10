# kvconf

Flat `key = value` config files for Go, with live reload. Standard library
only, no dependencies.

```ini
# ~/.config/myapp/config
theme      = dark
lock.after = 5m
border     = #ff8800          # a value may start with #
title      = "keeps # inside"
bind.cmd+q = close-window
output.DP-1.scale = 1.5
```

- **Typed decoding** into a struct with `kv` tags: strings, bools (`on/off`),
  numbers with bounds, durations, enums, lists, paths with `~/`, maps for
  dynamic keys (`bind.*`, `output.<name>.<field>`).
- **Live reload** with each OS's own notifications: inotify (Linux), kqueue
  (macOS, BSD), ReadDirectoryChangesW (Windows), or polling.
- **Safe for real configs**: atomic saves, symlinked dotfiles, editors that
  save by rename, missing directories, broken files that keep the last good
  config, and warnings that never print values (they may be secrets).
- **Fast**: about 750 MB/s to parse, 2 allocations per reported change
  (see [Performance](#performance)).

```sh
go get github.com/bnema/kvconf
```

## Quick start

```go
type Config struct {
	Theme  string            `kv:"theme" oneof:"dark|light"`
	Lock   time.Duration     `kv:"lock.after,off" max:"1h"`
	Shots  kvconf.Path       `kv:"screenshot.dir"`
	Binds  map[string]string `kv:"bind.*"`
}

func (c *Config) SetDefaults() { c.Theme, c.Lock = "dark", 5*time.Minute }

func main() {
	path, _ := kvconf.DefaultPath("myapp", "config")
	live, warnings, err := kvconf.Open(path, kvconf.LiveOptions[Config]{
		OnChange: func(c kvconf.Change[Config]) { log.Println("reloaded", c.Keys) },
		OnError:  func(err error) { log.Println("config:", err) },
	})
	if err != nil {
		log.Fatal(err)
	}
	for _, w := range warnings {
		log.Println("config:", w) // "line 3: theme: must be dark or light"
	}
	go live.Run(ctx)

	cfg := live.Get() // safe from any goroutine; never modified after
}
```

Without live reload: `cfg, warnings, err := kvconf.Load[Config](path, kvconf.Options{})`.

## File format

- One `key = value` per line. Spaces around the key and value are trimmed.
- `#` starts a comment at the start of a line, or after a space or a tab.
  `#ff8800` and `a#b` are values. See `Options.Comments` for other styles.
- A value that is exactly `"..."` (Go escapes) or `'...'` (literal) is
  unquoted. `spawn "a b"` stays as written.
- UTF-8, with an optional BOM and `\r\n` line ends.
- A repeated key overrides the earlier one, with a warning.

## Tags

| Tag | Meaning |
| --- | --- |
| `kv:"name"` | The key. A nested struct prefixes its keys with `name.` |
| `kv:",inline"` | A nested struct whose keys are not prefixed |
| `kv:"name,off"` | `off` sets the zero value |
| `kv:"name,nonempty"` | An empty value is rejected |
| `kv:"bind.*"` | A map: `bind.<name> = v`. A map of structs takes `bind.<name>.<field>`; names may contain dots |
| `kv:"*"` | `[]kvconf.Entry` or `map[string]string` collecting unknown keys |
| `min:"1" max:"10"` | Bounds of a number or duration (`max:"1h"`) |
| `oneof:"a\|b"` | Allowed values |
| `sep:"\|"` | List separator (default `,`) |

Any type implementing `encoding.TextUnmarshaler` works as a field.
`Validate() error` on the config checks rules spanning several keys.
`SetDefaults()` fills defaults, also for each new element of a map of structs.

## Errors and warnings

By default a bad line or value becomes a `Warning` with its line and key,
and the rest of the file still applies. `Options.Strict` rejects the whole
file instead. `Options.AllowUnknown`, `AllowDuplicates`, `RequireFile`,
`MaxBytes` (1 MiB) and `MaxLine` (64 KiB) tune the rest.

`Live` keeps the current config when the file is broken or removed, and
skips saves that change no value.

## Lower-level API

- `Parse`, `ParseBytes`: file to `[]Entry` without decoding.
- `Decode`, `Unmarshal`, `Marshal`: entries or bytes to and from a struct.
- `Watch(ctx, path, WatchOptions{...}, out)`: change notifications for one
  file, or every file of a directory with `Dir: true`.
- `Set`, `SetFile`: change keys in place, keeping comments and layout.
- `WriteFile`: atomic replace, written through symlinks.
- `ReadFile`: bounded read that rejects FIFOs and devices without blocking.

## Performance

Measured at `v0.1.1` (`ebc8127`, 2026-10-10) with Go 1.27.1 on linux/amd64
with an AMD Ryzen 9 7900X3D. Values are medians of 6 runs. The input is a
realistic ~2.9 KB config of 100 lines with comments, 60 binds and 8 outputs.

| Benchmark | Time/op | Throughput | Bytes/op | Allocs/op |
|---|---:|---:|---:|---:|
| `Parse` (file to entries) | 3.9 µs | 744 MB/s | 10.4 KiB | 5 |
| `Unmarshal` (file to struct, maps included) | 12.2 µs | 238 MB/s | 21.7 KiB | 47 |
| `Decode` (5 scalar entries) | 171 ns | | 112 B | 1 |
| `Marshal` (struct to file) | 11.6 µs | | 15.5 KiB | 19 |
| `Set` (edit one key in place) | 2.5 µs | | 10.5 KiB | 5 |
| `Watch` (one write to one report, inotify) | 5.3 µs | | 256 B | 2 |

Reproduce with:

```sh
go test -run '^$' -bench . -benchmem -count 6 .
```

## License

MIT
