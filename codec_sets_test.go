//go:build cgo

package goffmpeg

import (
	"errors"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// The FFmpeg a build loads holds exactly its codec set's decoders and
// demuxers (#41), read from the library rather than from the configure flags,
// which can bring in more than they name. check-components holds every build
// to the same lists at configure time, mobile targets included; this is the
// same check at run time, for the targets that run tests.
func TestBuiltCodecsAreTheSet(t *testing.T) {
	wantDecoders := []string{"mp3float", "pcm_f32le", "pcm_f64le", "pcm_s16le", "pcm_s24le", "pcm_s32le", "pcm_u8"}
	wantDemuxers := []string{"mp3", "wav"}
	if testCodecSet == "media" {
		wantDecoders = append(wantDecoders, "aac", "h264")
		// mov for MP4; hls and mpegts for HLS, with what hls selects.
		wantDemuxers = append(wantDemuxers, "mov", "hls", "mpegts", "aac", "ac3", "eac3")
		switch runtime.GOOS {
		case "darwin", "linux", "windows":
			wantDecoders = append(wantDecoders, "mjpeg", "rawvideo") // webcams
		case "android":
			// MediaCodec's H.264 decoder (goffmpeg-libs#21), for #65.
			wantDecoders = append(wantDecoders, "h264_mediacodec")
		}
	}
	decoders, demuxers := builtComponents()
	// The royalty-free formats (#78, goffmpeg-libs v0.9.0): Opus in Ogg in
	// both sets; VP9, AV1 (dav1d) and WebM in the media set, with the phones'
	// hardware decoders.
	wantDecoders = append(wantDecoders, "opus")
	wantDemuxers = append(wantDemuxers, "ogg")
	if testCodecSet == "media" {
		wantDecoders = append(wantDecoders, "vp9", "libdav1d", "av1")
		wantDemuxers = append(wantDemuxers, "matroska")
		if runtime.GOOS == "android" {
			wantDecoders = append(wantDecoders, "vp9_mediacodec", "av1_mediacodec")
		}
	}
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
		if everyCodec() {
			// The desktop media set has every codec: the list is a floor.
			for _, w := range c.want {
				if !slices.Contains(c.got, w) {
					t.Errorf("the desktop media set has no %s %s", c.kind, w)
				}
			}
			continue
		}
		if !slices.Equal(c.got, c.want) {
			t.Errorf("the %s set's %s are %s, want %s", testCodecSet, c.kind,
				strings.Join(c.got, " "), strings.Join(c.want, " "))
		}
	}
	// Hundreds, where a trimmed set has a dozen: v0.5.0's had 478.
	if everyCodec() && len(decoders) < 300 {
		t.Errorf("the desktop media set has %d decoders, want every codec FFmpeg has", len(decoders))
	}
}

// everyCodec reports a build that links the desktop media set, which has
// every codec FFmpeg has (owner, #41). The phone media set and the audio set
// have only their lists.
func everyCodec() bool {
	switch runtime.GOOS {
	case "darwin", "linux", "windows":
		return testCodecSet == "media"
	}
	return false
}

// A WAV whose codec a trimmed set doesn't decode, ADPCM, fails to open with
// ErrNoDecoder naming it (#41): the wav demuxer reads it, nothing decodes it.
// The desktop media set, which has every codec, plays it.
func TestNoDecoderForADPCM(t *testing.T) {
	requireMissingDecoderSupport(t)
	dec, err := Open("testdata/sample_adpcm.wav")
	if everyCodec() {
		if err != nil {
			t.Fatalf("the desktop media set didn't open an ADPCM WAV: %v", err)
		}
		dec.Close()
		return
	}
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
