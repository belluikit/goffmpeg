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
	if c, g := int(decoder_struct_size()), int(unsafe.Sizeof(cDecoder{})); c != g {
		t.Fatalf("sizeof(Decoder) = %d in C, unsafe.Sizeof(cDecoder{}) = %d in Go", c, g)
	}
}
