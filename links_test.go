//go:build cgo

package goffmpeg

import (
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"os"
	"path"
	"runtime"
	"strings"
	"testing"
)

// linkedFFmpeg is the set of FFmpeg libraries the test binary loads as shared
// libraries, by name ("avcodec", "swscale"): what a package has to carry
// beside the binary. The binary's own list of imported libraries is read,
// rather than its symbols, because a library linked by path is imported even
// when nothing calls it (#40). Only the -goffmpeg names count, so a system
// FFmpeg could not stand in for them. It skips where the format isn't read
// here.
func linkedFFmpeg(t *testing.T) map[string]bool {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var imports []string
	switch runtime.GOOS {
	case "darwin", "ios":
		f, err := macho.Open(exe)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		imports, err = f.ImportedLibraries()
		if err != nil {
			t.Fatal(err)
		}
	case "linux", "android":
		f, err := elf.Open(exe)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		imports, err = f.ImportedLibraries()
		if err != nil {
			t.Fatal(err)
		}
	case "windows":
		f, err := pe.Open(exe)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		imports, err = f.ImportedLibraries()
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Skipf("imported libraries not read on %s", runtime.GOOS)
	}
	// positive marker: every binary imports the C library, so an empty list
	// means the read found nothing rather than that nothing is imported
	if len(imports) == 0 {
		t.Fatal("the test binary imports no libraries: the read found nothing")
	}
	got := map[string]bool{}
	for _, lib := range imports {
		// @rpath/libavcodec-goffmpeg.63.dylib, @rpath/libavcodec-goffmpeg.framework/libavcodec-goffmpeg,
		// libavcodec-goffmpeg.so.63, avcodec-goffmpeg-63.dll
		base := path.Base(strings.ReplaceAll(lib, `\`, "/"))
		if name, _, ok := strings.Cut(strings.ToLower(base), "-goffmpeg"); ok {
			got[strings.TrimPrefix(name, "lib")] = true
		}
	}
	return got
}

// requireSharedFFmpeg is linkedFFmpeg for a binary that must link FFmpeg
// dynamically. A goffmpeg-libs older than the shared link (v0.3.x) links the
// static archives, so the binary imports none of them; that is a skip, not a
// failure, until go.mod requires a goffmpeg-libs with the shared link (#40).
func requireSharedFFmpeg(t *testing.T) map[string]bool {
	t.Helper()
	got := linkedFFmpeg(t)
	if len(got) == 0 {
		t.Skip("this goffmpeg-libs links FFmpeg statically: the binary imports no -goffmpeg library (#40)")
	}
	return got
}

// Every build loads FFmpeg as shared libraries (player ADR-0037 D3), so a
// user can replace them (LGPL 2.1 §6b), and none of FFmpeg's code is linked
// into the binary itself.
func TestLinksSharedFFmpeg(t *testing.T) {
	got := requireSharedFFmpeg(t)
	want := []string{"avformat", "avcodec", "swresample", "avutil"}
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" || runtime.GOOS == "windows" {
		want = append(want, "avdevice") // capture, desktop only
	}
	for _, lib := range want {
		if !got[lib] {
			t.Errorf("lib%s-goffmpeg isn't among the binary's imported libraries", lib)
		}
	}
	defined := definedSymbols(t)
	if defined == nil {
		return // no symbol table to read: the imports above are the check
	}
	for _, s := range []string{"avcodec_send_packet", "avformat_open_input", "swr_convert", "av_frame_alloc"} {
		if defined[s] {
			t.Errorf("%s is defined in the binary: FFmpeg is linked in, not loaded", s)
		}
	}
}

// definedSymbols is the test binary's symbols that it defines itself, not the
// ones it imports, or nil where there is no symbol table to read: a Linux test
// binary has no .symtab, and Windows' isn't read here.
func definedSymbols(t *testing.T) map[string]bool {
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
			t.Log("the test binary has no symbol table")
			return nil
		}
		for _, s := range f.Symtab.Syms {
			if s.Sect != 0 { // 0 is NO_SECT: undefined here
				names[strings.TrimPrefix(s.Name, "_")] = true
			}
		}
	case "linux", "android":
		f, err := elf.Open(exe)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		syms, err := f.Symbols()
		if err != nil {
			t.Logf("the test binary has no symbol table: %v", err)
			return nil
		}
		for _, s := range syms {
			if s.Section != elf.SHN_UNDEF {
				names[s.Name] = true
			}
		}
	default:
		t.Logf("defined symbols not read on %s", runtime.GOOS)
		return nil
	}
	// positive marker: the bridge is compiled into every cgo build
	if !names["decoder_open"] {
		t.Fatal("decoder_open isn't defined in the binary: the scan read nothing")
	}
	return names
}
