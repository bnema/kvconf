package kvconf

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

type benchConfig struct {
	Theme   string            `kv:"theme" oneof:"auto|dark|light"`
	Enabled bool              `kv:"enabled"`
	Count   int               `kv:"count" min:"0" max:"1000"`
	After   time.Duration     `kv:"lock.after,off"`
	Ratio   float64           `kv:"ratio"`
	Tags    []string          `kv:"tags"`
	Color   string            `kv:"border.color"`
	Binds   map[string]string `kv:"bind.*"`
	Outputs map[string]output `kv:"output.*"`
}

// benchFile is a realistic config: ~100 lines with comments, binds and
// per-output settings.
var benchFile = func() []byte {
	var b strings.Builder
	b.WriteString("# a comment\ntheme = dark\nenabled = on\ncount = 42\nlock.after = 5m\nratio = 1.5\n")
	b.WriteString("tags = a, b, c\nborder.color = #ff8800  # orange\n\n")
	for i := range 60 {
		fmt.Fprintf(&b, "bind.cmd+%d = spawn \"foot -e app%d\"\n", i, i)
	}
	for i := range 8 {
		fmt.Fprintf(&b, "output.DP-%d.scale = 1.25\noutput.DP-%d.hdr = off\noutput.DP-%d.sdr-brightness = 203\n", i, i, i)
	}
	return []byte(b.String())
}()

func BenchmarkParse(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(benchFile)))
	for b.Loop() {
		if _, _, err := ParseBytes(benchFile, Options{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkUnmarshal(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(benchFile)))
	for b.Loop() {
		var c benchConfig
		if w, err := Unmarshal(benchFile, &c, Options{}); err != nil || len(w) > 0 {
			b.Fatal(w, err)
		}
	}
}

func BenchmarkDecodeScalars(b *testing.B) {
	entries, _, _ := ParseBytes([]byte("theme = dark\nenabled = on\ncount = 42\nlock.after = 5m\nratio = 1.5\n"), Options{})
	b.ReportAllocs()
	for b.Loop() {
		var c benchConfig
		if _, err := Decode(entries, &c, Options{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMarshal(b *testing.B) {
	var c benchConfig
	if _, err := Unmarshal(benchFile, &c, Options{}); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Marshal(&c); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkWatchEvent measures one change seen by the watcher: the event,
// its classification and the debounced report.
func BenchmarkWatchEvent(b *testing.B) {
	dir := b.TempDir()
	path := dir + "/config"
	if err := os.WriteFile(path, benchFile, 0o600); err != nil {
		b.Fatal(err)
	}
	out := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	go func() { _ = Watch(ctx, path, WatchOptions{Debounce: -1, Ready: ready}, out) }()
	<-ready
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := f.WriteAt([]byte{'#'}, 0); err != nil {
			b.Fatal(err)
		}
		<-out
	}
}

func BenchmarkSet(b *testing.B) {
	values := map[string]*string{"output.DP-3.scale": ptr("2")}
	b.ReportAllocs()
	for b.Loop() {
		Set(benchFile, values)
	}
}
