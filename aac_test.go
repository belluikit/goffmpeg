//go:build cgo && !goffmpeg_novideo

package goffmpeg

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// aacClips are the AAC streams apps meet, each profile in each container, with
// what FFmpeg's aac decoder makes of them on macOS: the rate, the channel
// count and the length in samples per channel. Android decodes AAC with its
// own decoder (#77), and must make the same of them.
//
// testdata/aac was encoded from one 3 s, 44.1 kHz stereo source whose two
// channels differ and carry tones above 11 kHz, so a decoder that drops
// HE-AAC's high band (SBR) or its parametric stereo shows it: afconvert -d aac
// (128 kb/s), aach (48 kb/s) and aacp (32 kb/s) into M4A, and the LC and HE
// files copied into ADTS with ffmpeg -c copy -f adts. M4A says how much of the
// start and end is the encoder's padding, so those decode to exactly 3 s
// (132300 samples); ADTS cannot say, so those are longer.
var aacClips = []struct {
	path, what     string
	rate, channels int
	samples        int
	stereo         bool // two channels that differ
}{
	{"testdata/aac/lc.m4a", "AAC-LC stereo, M4A", 44100, 2, 132300, true},
	{"testdata/aac/he.m4a", "HE-AAC stereo, M4A", 44100, 2, 132300, true},
	{"testdata/aac/hev2.m4a", "HE-AACv2, M4A", 44100, 2, 132300, true},
	{"testdata/aac/lc.aac", "AAC-LC stereo, ADTS", 44100, 2, 135168, true},
	{"testdata/aac/he.aac", "HE-AAC stereo, ADTS", 44100, 2, 139264, true},
	{"testdata/sample_51.m4a", "AAC-LC 5.1, M4A", 44100, 6, 88200, false},
	{"testdata/sample.mp4", "AAC-LC mono, MP4 with H.264", 44100, 1, 176400, false},
	{"testdata/hls/stream.m3u8", "AAC-LC mono, HLS", 44100, 1, 90112, false},
}

// decodeAudio opens path and decodes its audio from seek seconds (0: from
// the start) to the end. It returns what AudioInfo reported, the decoder the
// audio opened with, the PCM, the position of the first audio frame, and how
// long Open took.
func decodeAudio(t *testing.T, path string, seek float64) (AudioInfo, string, []byte, float64, time.Duration) {
	t.Helper()
	start := time.Now()
	dec, err := Open(path)
	opened := time.Since(start)
	if err != nil {
		t.Fatalf("%s: Open: %v", path, err)
	}
	defer dec.Close()
	info := dec.AudioInfo()
	if seek > 0 {
		// Decode a little first, so the seek flushes a decoder that has
		// started.
		for n := 0; n < 10; n++ {
			if mt, err := dec.Next(); err != nil || mt == MediaTypeNone {
				t.Fatalf("%s: Next before the seek: %v %v", path, mt, err)
			}
		}
		if err := dec.Seek(seek); err != nil {
			t.Fatalf("%s: Seek(%v): %v", path, seek, err)
		}
	}
	var pcm []byte
	first := -1.0
	for {
		mt, err := dec.Next()
		if err != nil {
			t.Fatalf("%s: Next: %v", path, err)
		}
		if mt == MediaTypeNone {
			break
		}
		if mt != MediaTypeAudio {
			continue
		}
		if first < 0 {
			first = dec.Position()
		}
		b, err := dec.GetAudio()
		if err != nil {
			t.Fatalf("%s: GetAudio: %v", path, err)
		}
		pcm = append(pcm, b...)
	}
	return info, dec.(interface{ audioDecoder() string }).audioDecoder(), pcm, first, opened
}

// stereoWidth is the energy of the difference between the first two channels
// over that of both: 0 for identical channels, about 1 for unrelated ones.
func stereoWidth(pcm []byte, channels int) float64 {
	if channels < 2 {
		return 0
	}
	var diff, all float64
	for i := 0; i+2*channels <= len(pcm); i += 2 * channels {
		l := float64(int16(binary.LittleEndian.Uint16(pcm[i:])))
		r := float64(int16(binary.LittleEndian.Uint16(pcm[i+2:])))
		diff += (l - r) * (l - r)
		all += l*l + r*r
	}
	if all == 0 {
		return 0
	}
	return diff / all
}

func rms(pcm []byte) float64 {
	var sum float64
	n := len(pcm) / 2
	for i := 0; i < n; i++ {
		v := float64(int16(binary.LittleEndian.Uint16(pcm[2*i:])))
		sum += v * v
	}
	if n == 0 {
		return 0
	}
	return math.Sqrt(sum / float64(n))
}

// Each AAC clip decodes to the rate, channels and length FFmpeg's aac decoder
// gives it, and a stereo clip stays stereo. On Android that is Android's own
// decoder (#77), so this holds it to FFmpeg's, through the container's
// padding (patches/0003), ADTS and HLS (patches/0002), HE-AAC's high band and
// HE-AACv2's parametric stereo. With GOFFMPEG_TEST_AAC_DUMP set to a
// directory, each clip's PCM is written there as well, for comparing the two
// decoders' samples, which this cannot do on a phone.
func TestAACClips(t *testing.T) {
	if testCodecSet != "media" {
		t.Skip("AAC is in the media set")
	}
	want := "aac"
	if runtime.GOOS == "android" {
		want = "aac_mediacodec"
	}
	dump := os.Getenv("GOFFMPEG_TEST_AAC_DUMP")
	for _, c := range aacClips {
		info, decoder, pcm, _, opened := decodeAudio(t, c.path, 0)
		samples := 0
		if info.Channels > 0 {
			samples = len(pcm) / 2 / info.Channels
		}
		t.Logf("%s: %s, %d Hz, %d channels, %d samples, sha256 %x, open %v (%s)", c.path, decoder,
			info.SampleRate, info.Channels, samples, sha256.Sum256(pcm), opened.Round(time.Millisecond), c.what)
		if decoder != want {
			t.Errorf("%s: decoded by %q, want %q", c.path, decoder, want)
		}
		if info.SampleRate != c.rate || info.Channels != c.channels || samples != c.samples {
			t.Errorf("%s (%s): %d Hz, %d channels, %d samples; want %d Hz, %d channels, %d samples",
				c.path, c.what, info.SampleRate, info.Channels, samples, c.rate, c.channels, c.samples)
		}
		if w := stereoWidth(pcm, info.Channels); c.stereo && w < 0.5 {
			t.Errorf("%s (%s): the channels are alike (width %.3f): stereo was lost", c.path, c.what, w)
		}
		if dump != "" {
			name := strings.ReplaceAll(strings.TrimPrefix(c.path, "testdata/"), "/", "_")
			out := filepath.Join(dump, fmt.Sprintf("%s.%s.%dch.%d.s16", name, decoder, info.Channels, info.SampleRate))
			if err := os.WriteFile(out, pcm, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// After a seek, each AAC clip decodes from where the seek landed to its end:
// the samples left match what the position says is left, and are not
// silence. On Android this flushes MediaCodec, which for ADTS and HLS must be
// given the stream's configuration again (patches/0002).
//
// Positions are the stream's own timestamps, and the HLS clip's start at
// 1.457 s, so what is left is counted from the clip's first position. M4A
// says how much padding the encoder put at the start of the stream, not after
// a seek, so there a decoder's priming comes out with the audio: measured with
// FFmpeg's aac on macOS, 2112 samples for AAC-LC and 4224 for HE-AAC, and
// none from ADTS, which says nothing.
func TestAACSeek(t *testing.T) {
	if testCodecSet != "media" {
		t.Skip("AAC is in the media set")
	}
	const into = 1.0 // seconds after the clip's first position
	for _, c := range aacClips {
		_, _, _, start, _ := decodeAudio(t, c.path, 0)
		seek := start + into
		info, _, pcm, first, _ := decodeAudio(t, c.path, seek)
		if info.Channels <= 0 || first < 0 {
			t.Errorf("%s: no audio after Seek(%v)", c.path, seek)
			continue
		}
		got := len(pcm) / 2 / info.Channels
		left := c.samples - int(math.Round((first-start)*float64(c.rate)))
		t.Logf("%s: Seek(%.3f) from %.3f, first frame at %.3f s, %d samples after it, %d left by that position (%+d)",
			c.path, seek, start, first, got, left, got-left)
		// A seek lands at or before its target: on the video's keyframe in
		// sample.mp4 (0.906 s), and at the start of the segment that holds
		// it in HLS, whose segments are 1 s long.
		if first < seek-1.05 || first > seek+0.05 {
			t.Errorf("%s: Seek(%.3f) landed at %.3f s", c.path, seek, first)
		}
		if d := got - left; d < -1024 || d > 4224 {
			t.Errorf("%s: %d samples after the seek, but %d were left from %.3f s", c.path, got, left, first)
		}
		if r := rms(pcm); r < 100 {
			t.Errorf("%s: the audio after the seek is silence (RMS %.1f)", c.path, r)
		}
	}
}
