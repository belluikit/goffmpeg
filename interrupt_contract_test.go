package goffmpeg

import (
	"errors"
	"strings"
	"syscall"
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

// TestSeekResultWrapsAVError pins that a failed Seek carries FFmpeg's code as
// an AVError, so a caller can recognise a stream that cannot seek (ENOSYS)
// rather than parse "seek failed: -38" (bellui#1098).
func TestSeekResultWrapsAVError(t *testing.T) {
	var s interruptState
	enosys := -int(syscall.ENOSYS)
	err := s.seekResult(enosys)
	var averr AVError
	if !errors.As(err, &averr) || int(averr) != enosys {
		t.Fatalf("seekResult(%d) = %v; want an AVError(%d) inside", enosys, err, enosys)
	}
	if !strings.HasPrefix(err.Error(), "seek failed: ") {
		t.Errorf("message %q lost its prefix", err)
	}
	if s.seekResult(0) != nil {
		t.Error("seekResult(0) reported a failure")
	}
	s.set.Store(true)
	if !errors.As(s.seekResult(0), &averr) || averr != avErrorExit {
		t.Errorf("an interrupted seek does not report AVERROR_EXIT")
	}
}
