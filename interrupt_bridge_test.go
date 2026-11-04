package goffmpeg

import "testing"

// bridgeCaller reaches the bridge's decoder_next and decoder_seek beneath the
// Go-side interrupt check. Both backends implement it: cgo calls the bridge
// compiled from this source, purego the libgoffmpeg it loaded from
// goffmpeg-libs (which has the check from v0.3.5).
type bridgeCaller interface {
	bridgeNext() int
	bridgeSeek(seconds float64) int
}

// TestBridgeRefusesCallsAfterInterrupt checks the bridge on its own, beneath
// the Go-side check, so the contract holds for any caller of the bridge (#37).
func TestBridgeRefusesCallsAfterInterrupt(t *testing.T) {
	dec, err := Open("testdata/sample_cover.mp3")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer dec.Close()
	d := dec.(bridgeCaller)
	if res := d.bridgeNext(); res < 0 {
		t.Fatalf("decoder_next before Interrupt = %d", res)
	}

	dec.Interrupt()
	if res := d.bridgeSeek(0.5); res != int(avErrorExit) {
		t.Errorf("decoder_seek after Interrupt = %d; want AVERROR_EXIT (%d)", res, int(avErrorExit))
	}
	if res := d.bridgeNext(); res != int(avErrorExit) {
		t.Errorf("decoder_next after Interrupt = %d; want AVERROR_EXIT (%d)", res, int(avErrorExit))
	}
}
