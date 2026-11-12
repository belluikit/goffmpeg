//go:build !cgo

package goffmpeg

import (
	"testing"
	"unsafe"
)

// TestDecoderStructSize guards cDecoder against drifting from the C Decoder it
// mirrors. The C side memsets sizeof(Decoder) bytes over the Go allocation, so
// a field added in bridge.h and not here writes past the end of it. Field
// order and widths matter as much as the total, but a missing or wrongly sized
// field changes the size, which is what this catches.
func TestDecoderStructSize(t *testing.T) {
	if decoder_struct_size == nil {
		t.Skip("libgoffmpeg predates decoder_struct_size (#24); rebuild it to run this")
	}
	g := int(unsafe.Sizeof(cDecoder{}))
	// A library from before decoder_interrupt has no interrupt field; its
	// struct ends where that field begins. The Go mirror being larger than
	// such a library's struct is safe: its memset covers less.
	// Likewise a library from before #65, which has no hw field.
	if decoder_interrupt == nil {
		g = int(unsafe.Offsetof(cDecoder{}.interrupt))
	} else if decoder_video_decoder == nil {
		g = int(unsafe.Offsetof(cDecoder{}.hw))
	}
	if c := int(decoder_struct_size()); c != g {
		t.Fatalf("sizeof(Decoder) = %d in C, %d in Go", c, g)
	}
}
