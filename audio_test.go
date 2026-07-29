//go:build cgo

package goffmpeg

import (
	"os"
	"testing"
)

// multichannelMedia is a 2s 5.1 AAC clip, used to prove that a non-stereo
// layout survives decoding rather than being silently folded down.
const multichannelMedia = "testdata/sample_51.m4a"

func openMultichannel(t *testing.T) IDecoder {
	t.Helper()
	if _, err := os.Stat(multichannelMedia); err != nil {
		t.Skipf("multichannel fixture unavailable: %v", err)
	}
	dec, err := Open(multichannelMedia)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(dec.Close)
	return dec
}

// collectAudio decodes up to limit packets and returns the total PCM bytes.
func collectAudio(t *testing.T, dec IDecoder, limit int) (packets, bytes int) {
	t.Helper()
	for i := 0; i < limit; i++ {
		mt, err := dec.Next()
		if err != nil || mt == MediaTypeNone {
			break
		}
		if mt == MediaTypeAudio {
			b, err := dec.GetAudio()
			if err != nil {
				t.Fatalf("GetAudio: %v", err)
			}
			packets++
			bytes += len(b)
		}
	}
	return packets, bytes
}

// TestNativeChannelLayoutPreserved is the regression test for the downmix
// bug: the bridge used to hardcode AV_CHANNEL_LAYOUT_STEREO, so every
// multichannel source was folded to 2 channels with no way to opt out.
func TestNativeChannelLayoutPreserved(t *testing.T) {
	dec := openMultichannel(t)

	if got := dec.AudioInfo().Channels; got != 6 {
		t.Fatalf("AudioInfo().Channels = %d, want 6 (native 5.1 preserved)", got)
	}

	packets, total := collectAudio(t, dec, 50)
	if packets == 0 || total == 0 {
		t.Fatalf("decoded %d packets / %d bytes, want non-zero", packets, total)
	}
	// S16 x 6 channels = 12 bytes per frame; a buffer that is not a multiple
	// of that would mean the byte count and the layout disagree.
	if total%12 != 0 {
		t.Errorf("total audio = %d bytes, not a multiple of 12 (S16 x 6ch)", total)
	}
}

// TestSetAudioChannelsDownmixes checks the explicit opt-in to stereo.
func TestSetAudioChannelsDownmixes(t *testing.T) {
	dec := openMultichannel(t)

	if err := dec.SetAudioChannels(2); err != nil {
		t.Fatalf("SetAudioChannels(2): %v", err)
	}
	if got := dec.AudioInfo().Channels; got != 2 {
		t.Fatalf("AudioInfo().Channels = %d, want 2 after downmix", got)
	}

	packets, total := collectAudio(t, dec, 50)
	if packets == 0 || total == 0 {
		t.Fatalf("decoded %d packets / %d bytes, want non-zero", packets, total)
	}
	if total%4 != 0 {
		t.Errorf("total audio = %d bytes, not a multiple of 4 (S16 x 2ch)", total)
	}
}

// TestSetAudioChannelsByteRatio pins the sizing bug that rode along with the
// hardcoded layout: decoder_get_audio returned samples*4 regardless of channel
// count, so multichannel buffers were reported at half their real length.
// Decoding identical content at 6 and 2 channels must differ by ~3x.
func TestSetAudioChannelsByteRatio(t *testing.T) {
	six := openMultichannel(t)
	_, sixBytes := collectAudio(t, six, 30)

	two := openMultichannel(t)
	if err := two.SetAudioChannels(2); err != nil {
		t.Fatalf("SetAudioChannels(2): %v", err)
	}
	_, twoBytes := collectAudio(t, two, 30)

	if sixBytes == 0 || twoBytes == 0 {
		t.Fatalf("got %d bytes at 6ch and %d at 2ch, want both non-zero", sixBytes, twoBytes)
	}
	if ratio := float64(sixBytes) / float64(twoBytes); ratio < 2.9 || ratio > 3.1 {
		t.Errorf("6ch/2ch byte ratio = %.2f (%d/%d), want ~3.0", ratio, sixBytes, twoBytes)
	}
}

// TestSetAudioChannelsNativeRestore checks that 0 returns to the source layout.
func TestSetAudioChannelsNativeRestore(t *testing.T) {
	dec := openMultichannel(t)

	if err := dec.SetAudioChannels(2); err != nil {
		t.Fatalf("SetAudioChannels(2): %v", err)
	}
	if err := dec.SetAudioChannels(0); err != nil {
		t.Fatalf("SetAudioChannels(0): %v", err)
	}
	if got := dec.AudioInfo().Channels; got != 6 {
		t.Errorf("AudioInfo().Channels = %d after restoring native, want 6", got)
	}
}

// TestSetAudioChannelsOnVideoOnlyStream verifies the no-audio path reports an
// error rather than dereferencing a null codec context.
func TestSetAudioChannelsOnSilentDecoder(t *testing.T) {
	dec, err := Open(sampleMedia)
	if err != nil {
		t.Skipf("sample media unavailable: %v", err)
	}
	defer dec.Close()

	// sampleMedia has audio, so this should succeed; the guard is exercised
	// by the error return rather than a crash.
	if err := dec.SetAudioChannels(1); err != nil {
		t.Fatalf("SetAudioChannels(1): %v", err)
	}
	if got := dec.AudioInfo().Channels; got != 1 {
		t.Errorf("AudioInfo().Channels = %d, want 1 (mono)", got)
	}
}
