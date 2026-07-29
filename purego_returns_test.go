//go:build !cgo

package goffmpeg

import "testing"

// TestNegativeCIntArrivesNegative guards the binding widths in
// decoder_purego.go.
//
// The bridge returns C int, which is 32 bits. purego reads the full 64-bit
// return register, and on arm64 a C int comes back in w0 with the upper half of
// x0 unspecified — so binding these as Go int delivers a negative value
// zero-extended: -1 arrives as 4294967295. Every `if res < 0` in this file then
// becomes dead code, and errors are silently treated as success.
//
// decoder_set_audio_channels is the cheapest probe: it returns -1 immediately
// when the decoder has no audio context, which a zero-value cDecoder
// guarantees, so nothing needs opening.
func TestNegativeCIntArrivesNegative(t *testing.T) {
	if decoder_set_audio_channels == nil {
		t.Skip("libgoffmpeg not loaded")
	}
	var d cDecoder // a_ctx == NULL, so the C side returns -1

	if got := decoder_set_audio_channels(&d, 2); got >= 0 {
		t.Fatalf("C returned -1 but purego delivered %d (0x%x) — the binding is "+
			"declared too wide, so negative returns are zero-extended and every "+
			"error check on this path is dead", got, uint64(got))
	}
}

// TestSetAudioChannelsReportsError is the same defect seen through the public
// API: without the fix this returned nil, reporting success for a failure.
func TestSetAudioChannelsReportsError(t *testing.T) {
	if decoder_set_audio_channels == nil {
		t.Skip("libgoffmpeg not loaded")
	}
	d := &puregoDecoder{} // no audio context

	if err := d.SetAudioChannels(2); err == nil {
		t.Error("SetAudioChannels reported success on a decoder with no audio")
	}
}
