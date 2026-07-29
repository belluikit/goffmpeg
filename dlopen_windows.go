//go:build !cgo && windows

package goffmpeg

import "syscall"

// loadLibrary opens a DLL by path or by name.
//
// purego provides Dlopen only on Unix-like systems — dlfcn.go is tagged
// (darwin || freebsd || linux || netbsd) — so Windows has to go through
// LoadLibrary and hand the resulting handle to RegisterLibFunc.
func loadLibrary(name string) (uintptr, error) {
	h, err := syscall.LoadLibrary(name)
	if err != nil {
		return 0, err
	}
	return uintptr(h), nil
}

// hasSymbol reports whether the DLL exports name. Used to probe before
// RegisterLibFunc, which panics on a missing symbol.
//
// purego.Dlsym is Unix-only for the same reason as Dlopen, so this goes
// through GetProcAddress.
func hasSymbol(lib uintptr, name string) bool {
	_, err := syscall.GetProcAddress(syscall.Handle(lib), name)
	return err == nil
}
