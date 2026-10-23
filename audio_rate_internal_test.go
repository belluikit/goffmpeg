package goffmpeg

import "testing"

// These drive audioRequest.setRate directly: it is shared by the cgo and
// purego backends, and the purego one cannot run without a built bridge.

func TestSetRateNoAudio(t *testing.T) {
	var a audioRequest
	var outRate int32 // a source without audio: the bridge leaves it 0
	called := false
	err := a.setRate(48000, &outRate, func(int) int { called = true; return 0 })
	if err == nil || called || outRate != 0 {
		t.Fatalf("err=%v rebuilt=%v outRate=%d, want an error, no rebuild, 0", err, called, outRate)
	}
}

func TestSetRateKeepsChannelsAndRestores(t *testing.T) {
	a := audioRequest{channels: 2}
	outRate := int32(44100)
	var got []int
	rebuild := func(ch int) int { got = append(got, ch); return 0 }

	if err := a.setRate(48000, &outRate, rebuild); err != nil || outRate != 48000 {
		t.Fatalf("setRate(48000): err=%v outRate=%d", err, outRate)
	}
	if err := a.setRate(0, &outRate, rebuild); err != nil || outRate != 44100 {
		t.Fatalf("setRate(0): err=%v outRate=%d, want the native 44100", err, outRate)
	}
	if len(got) != 2 || got[0] != 2 || got[1] != 2 {
		t.Fatalf("rebuilt with channels %v, want [2 2]", got)
	}
}

func TestSetRateFailureRestoresPreviousRate(t *testing.T) {
	var a audioRequest
	outRate := int32(44100)
	var rates []int32
	rebuild := func(int) int {
		rates = append(rates, outRate)
		if outRate == 1 {
			return -22
		}
		return 0
	}
	if err := a.setRate(1, &outRate, rebuild); err == nil {
		t.Fatal("setRate(1) with a failing rebuild returned nil")
	}
	if outRate != 44100 {
		t.Fatalf("outRate = %d after a failed rebuild, want 44100 restored", outRate)
	}
	if len(rates) != 2 || rates[1] != 44100 {
		t.Fatalf("rebuilds at %v, want a second rebuild at 44100", rates)
	}
}
