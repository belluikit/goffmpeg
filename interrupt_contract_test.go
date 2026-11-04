package goffmpeg

import (
	"errors"
	"testing"
)

// TestInterruptedDecoderRefusesLaterCalls pins the half of Interrupt's contract
// that FFmpeg does not keep by itself: once interrupted, Seek and Next fail.
// FFmpeg consults the interrupt flag only on some I/O paths, so a local file
// went on seeking and decoding as if nothing had happened, and over http a
// seek interrupted at the wrong moment could land on an earlier index entry
// and report success (#37).
func TestInterruptedDecoderRefusesLaterCalls(t *testing.T) {
	d, err := Open("testdata/sample_cover.mp3")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	if _, err := d.Next(); err != nil {
		t.Fatalf("Next before Interrupt: %v", err)
	}

	d.Interrupt()
	if err := d.Seek(0.5); err == nil {
		t.Error("Seek after Interrupt succeeded; want a failure")
	}
	mt, err := d.Next()
	if err == nil {
		t.Errorf("Next after Interrupt = %v, nil; want a failure", mt)
	}
	var averr AVError
	if !errors.As(err, &averr) || averr != avErrorExit {
		t.Errorf("Next after Interrupt: %v; want AVERROR_EXIT, as when FFmpeg notices the interrupt itself", err)
	}
	// Interrupt after Close does nothing, so it must not panic either.
	d.Close()
	d.Interrupt()
}
