//go:build !cgo && !windows

package goffmpeg

import "github.com/ebitengine/purego"

// loadLibrary opens a shared library by path or by name, letting the platform
// loader search when the path does not resolve.
func loadLibrary(name string) (uintptr, error) {
	return purego.Dlopen(name, purego.RTLD_NOW|purego.RTLD_GLOBAL)
}

// hasSymbol reports whether the library exports name. Used to probe before
// RegisterLibFunc, which panics on a missing symbol.
func hasSymbol(lib uintptr, name string) bool {
	_, err := purego.Dlsym(lib, name)
	return err == nil
}
