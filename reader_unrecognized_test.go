//go:build cgo

package goffmpeg

import (
	"bytes"
	"errors"
	"testing"
)

// TestOpenUnrecognizedInput through OpenReader, which only the cgo build has.
func TestOpenReaderUnrecognizedInput(t *testing.T) {
	dec, err := OpenReader(bytes.NewReader(make([]byte, 64<<10)))
	if err == nil {
		dec.Close()
		t.Fatal("OpenReader opened a reader of zeros")
	}
	if !errors.Is(err, ErrUnrecognizedInput) {
		t.Errorf("OpenReader: %v, want ErrUnrecognizedInput", err)
	}
	var av AVError
	if !errors.As(err, &av) || av != avErrorInvalidData {
		t.Errorf("OpenReader: %v carries no AVERROR_INVALIDDATA", err)
	}
}
