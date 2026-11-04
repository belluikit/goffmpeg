//go:build cgo

// cgo only: it calls the bridge below the Go-side check, and only the cgo
// backend compiles the bridge from this source. The purego leg loads
// libgoffmpeg from goffmpeg-libs, which gets these checks only once those
// libraries are rebuilt from this bridge.

package goffmpeg

import "testing"

// TestBridgeRefusesCallsAfterInterrupt checks the bridge on its own, beneath
// the Go-side check, so the contract holds for any caller of the bridge (#37).
func TestBridgeRefusesCallsAfterInterrupt(t *testing.T) {
	dec, err := Open("testdata/sample_cover.mp3")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer dec.Close()
	d := dec.(*cgoDecoder)
	if res := d.bridgeNext(); res < 0 {
		t.Fatalf("decoder_next before Interrupt = %d", res)
	}

	d.Interrupt()
	if res := d.bridgeSeek(0.5); res != int(avErrorExit) {
		t.Errorf("decoder_seek after Interrupt = %d; want AVERROR_EXIT (%d)", res, int(avErrorExit))
	}
	if res := d.bridgeNext(); res != int(avErrorExit) {
		t.Errorf("decoder_next after Interrupt = %d; want AVERROR_EXIT (%d)", res, int(avErrorExit))
	}
}
