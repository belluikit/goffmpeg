//go:build cgo

package goffmpeg

import (
	"debug/elf"
	"debug/macho"
	"os"
	"runtime"
	"strings"
	"testing"
)

// linkedSymbols is the test binary's own symbol table, the names without
// macOS's leading underscore: what the static link pulled in. It skips where
// the format isn't read here.
func linkedSymbols(t *testing.T) map[string]bool {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	switch runtime.GOOS {
	case "darwin", "ios":
		f, err := macho.Open(exe)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if f.Symtab == nil {
			t.Skip("the test binary has no symbol table")
		}
		for _, s := range f.Symtab.Syms {
			names[strings.TrimPrefix(s.Name, "_")] = true
		}
	case "linux", "android":
		f, err := elf.Open(exe)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		syms, err := f.Symbols()
		if err != nil {
			t.Skipf("the test binary has no symbol table: %v", err)
		}
		for _, s := range syms {
			names[s.Name] = true
		}
	default:
		t.Skipf("symbols not read on %s", runtime.GOOS)
	}
	// positive marker: libswresample is linked in every build, so a table
	// without it means the scan read nothing
	if !names["swr_convert"] {
		t.Fatal("swr_convert isn't among the binary's symbols: the scan read nothing")
	}
	return names
}
