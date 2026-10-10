package kvconf

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ReadFile reads a config file safely: it must be a regular file (a FIFO or
// device is rejected without blocking) no larger than opts.MaxBytes. A
// missing file returns an error wrapping fs.ErrNotExist.
func ReadFile(path string, opts Options) ([]byte, error) {
	f, err := openNonblocking(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read only
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, &os.PathError{Op: "read", Path: path, Err: ErrNotRegular}
	}
	data, err := readLimited(f, opts.maxBytes())
	if errors.Is(err, ErrTooLarge) {
		return nil, fmt.Errorf("%w: %s", ErrTooLarge, path)
	}
	return data, err
}

// DefaultPath returns <config dir>/<app>/<name>. The config dir is
// $XDG_CONFIG_HOME when it is an absolute path; otherwise ~/.config on Unix,
// ~/Library/Application Support on macOS and %AppData% on Windows.
func DefaultPath(app, name string) (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, app, name), nil
	}
	var dir string
	switch runtime.GOOS {
	case "darwin", "ios", "windows", "plan9":
		d, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		dir = d
	default:
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, app, name), nil
}

// Path is a file system path value: it accepts an absolute path or one
// starting with ~/ (the home directory), and stores it cleaned. Use it as a
// field type to validate paths while decoding.
type Path string

// UnmarshalText implements encoding.TextUnmarshaler.
func (p *Path) UnmarshalText(text []byte) error {
	s, err := ExpandPath(string(text))
	if err != nil {
		return err
	}
	*p = Path(s)
	return nil
}

// MarshalText implements encoding.TextMarshaler.
func (p Path) MarshalText() ([]byte, error) { return []byte(p), nil }

// ExpandPath expands a leading ~/ and requires an absolute, NUL-free path.
func ExpandPath(s string) (string, error) {
	if s == "" || strings.IndexByte(s, 0) >= 0 {
		return "", ValueError("must be a non-empty path")
	}
	if rest, ok := strings.CutPrefix(s, "~/"); ok || s == "~" {
		if !ok {
			rest = ""
		}
		home, err := os.UserHomeDir()
		if err != nil || !filepath.IsAbs(home) {
			return "", ValueError("no absolute home directory for ~")
		}
		s = filepath.Join(home, rest)
	}
	if !filepath.IsAbs(s) {
		return "", ValueError("must be an absolute path or start with ~/")
	}
	return filepath.Clean(s), nil
}

// ErrNotRegular is returned by ReadFile for a directory, FIFO or device.
var ErrNotRegular = errors.New("not a regular file")

// isNotExist reports a missing file, including a missing parent directory.
func isNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }
