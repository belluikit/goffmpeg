//go:build cgo

package goffmpeg

import (
	"errors"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// requireCodecSets skips against a goffmpeg-libs built before the codec sets
// (#41), whose FFmpeg has every decoder: hundreds, where a set has a dozen.
// Once go.mod requires a goffmpeg-libs with the sets, this skip goes.
func requireCodecSets(t *testing.T) {
	t.Helper()
	if decoders, _ := builtComponents(); len(decoders) > 50 {
		t.Skipf("this goffmpeg-libs predates the codec sets: its FFmpeg has %d decoders (#41)", len(decoders))
	}
}

// The FFmpeg a build loads holds exactly its codec set's decoders and
// demuxers (#41), read from the library rather than from the configure flags,
// which can bring in more than they name. check-components holds every build
// to the same lists at configure time, mobile targets included; this is the
// same check at run time, for the targets that run tests.
func TestBuiltCodecsAreTheSet(t *testing.T) {
	requireCodecSets(t)
	wantDecoders := []string{"mp3float", "pcm_f32le", "pcm_f64le", "pcm_s16le", "pcm_s24le", "pcm_s32le", "pcm_u8"}
	wantDemuxers := []string{"mp3", "wav"}
	if testCodecSet == "media" {
		wantDecoders = append(wantDecoders, "aac", "h264")
		wantDemuxers = append(wantDemuxers, "mov")
		switch runtime.GOOS {
		case "darwin", "linux", "windows":
			wantDecoders = append(wantDecoders, "mjpeg", "rawvideo") // webcams
		}
	}
	decoders, demuxers := builtComponents()
	// A demuxer's name can be a list of the formats it reads: configure's
	// mov is "mov,mp4,m4a,3gp,3g2,mj2" at run time. Its first is the name.
	for i, d := range demuxers {
		demuxers[i], _, _ = strings.Cut(d, ",")
	}
	for _, c := range []struct {
		kind      string
		got, want []string
	}{{"decoders", decoders, wantDecoders}, {"demuxers", demuxers, wantDemuxers}} {
		slices.Sort(c.got)
		slices.Sort(c.want)
		if !slices.Equal(c.got, c.want) {
			t.Errorf("the %s set's %s are %s, want %s", testCodecSet, c.kind,
				strings.Join(c.got, " "), strings.Join(c.want, " "))
		}
	}
}

// A WAV whose codec neither set decodes, ADPCM, fails to open with
// ErrNoDecoder naming it (#41): the wav demuxer reads it, nothing decodes it.
func TestNoDecoderForADPCM(t *testing.T) {
	requireMissingDecoderSupport(t)
	requireCodecSets(t)
	dec, err := Open("testdata/sample_adpcm.wav")
	if err == nil {
		dec.Close()
		t.Fatal("an ADPCM WAV opened")
	}
	if !errors.Is(err, ErrNoDecoder) {
		t.Fatalf("%v, want ErrNoDecoder", err)
	}
	if !strings.Contains(err.Error(), "audio adpcm_ms") {
		t.Errorf("%q doesn't name the audio codec, adpcm_ms", err)
	}
}

// A PCM WAV plays in both sets.
func TestCodecSetPlaysWAV(t *testing.T) {
	dec, err := Open("testdata/sample.wav") // PCM s16le, 16 kHz mono
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	if a := dec.AudioInfo(); a.SampleRate != 16000 || a.Channels != 1 {
		t.Errorf("AudioInfo %+v, want 16000 Hz mono", a)
	}
	var bytes int
	for i := 0; i < 500; i++ {
		mt, err := dec.Next()
		if err != nil {
			t.Fatal(err)
		}
		if mt == MediaTypeNone {
			break
		}
		if b, err := dec.GetAudio(); err == nil {
			bytes += len(b)
		}
	}
	// 0.5 s at 16 kHz, one channel of S16: 16000 bytes.
	if bytes != 16000 {
		t.Errorf("decoded %d bytes of audio, want 16000", bytes)
	}
}
