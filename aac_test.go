//go:build cgo

package goffmpeg

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// aacClips are the AAC streams apps meet, each profile in each container, with
// what FFmpeg's aac decoder makes of them on macOS: the rate, the channel
// count and the length in samples per channel. Android and iOS decode AAC
// with their own decoders (#77), and must make the same of them.
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

// aacDecoder is the decoder each platform decodes AAC with: Android's and
// Apple's own on the phones (#77), FFmpeg's on desktop.
func aacDecoder() string {
	switch runtime.GOOS {
	case "android":
		return "aac_mediacodec"
	case "ios":
		return "aac_at"
	}
	return "aac"
}

// aacTestClips are the aacClips the loaded set reads: every one in the media
// set; in the audio set, which has AAC in M4A, MP4 and ADTS but no HLS (#77
// decision C), all but the HLS one. It skips the test on an audio set that
// predates AAC, as the published goffmpeg-libs' does until their next release.
func aacTestClips(t *testing.T) []string {
	t.Helper()
	var paths []string
	for _, c := range aacClips {
		paths = append(paths, c.path)
	}
	if testCodecSet == "media" {
		return paths
	}
	_, demuxers := builtComponents()
	if !slices.ContainsFunc(demuxers, func(d string) bool { return strings.HasPrefix(d, "mov,") }) {
		t.Skip("the loaded audio set predates AAC (#77)")
	}
	return slices.DeleteFunc(paths, func(p string) bool { return strings.HasSuffix(p, ".m3u8") })
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
// gives it, and a stereo clip stays stereo. On the phones that is their own
// decoder (#77), so this holds it to FFmpeg's: Android's through the
// container's padding (patches/0003), ADTS and HLS (patches/0002), Apple's
// through HE-AAC's high band and HE-AACv2's parametric stereo, signalled or
// not (patches/0005), and both through those. With GOFFMPEG_TEST_AAC_DUMP set to a
// directory, each clip's PCM is written there as well, for comparing the two
// decoders' samples, which this cannot do on a phone.
func TestAACClips(t *testing.T) {
	paths := aacTestClips(t)
	want := aacDecoder()
	dump := os.Getenv("GOFFMPEG_TEST_AAC_DUMP")
	for _, c := range aacClips {
		if !slices.Contains(paths, c.path) {
			continue
		}
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
	paths := aacTestClips(t)
	const into = 1.0 // seconds after the clip's first position
	for _, c := range aacClips {
		if !slices.Contains(paths, c.path) {
			continue
		}
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

// A seek back to the start after the end of each AAC clip decodes the clip
// again, as a player that loops does (#103). At the end of the file the
// bridge drains the decoder with an empty packet. Apple's aac_at kept that
// end-of-stream state through the flush a seek does, and gave no audio after
// it (patches/0010): a looping player on an iPad went silent, and its video,
// paced by the audio clock, played 1.6 frames a second. The second pass has
// the first's samples, give or take the priming a seek lets out (TestAACSeek).
func TestAACSeekAfterEnd(t *testing.T) {
	paths := aacTestClips(t)
	for _, c := range aacClips {
		if !slices.Contains(paths, c.path) {
			continue
		}
		dec, err := Open(c.path)
		if err != nil {
			t.Fatalf("%s: Open: %v", c.path, err)
		}
		// pass decodes to the end and returns the samples per channel, the
		// first audio frame's position, and the PCM.
		pass := func() (int, float64, []byte) {
			var pcm []byte
			first := -1.0
			for {
				mt, err := dec.Next()
				if err != nil {
					t.Fatalf("%s: Next: %v", c.path, err)
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
					t.Fatalf("%s: GetAudio: %v", c.path, err)
				}
				pcm = append(pcm, b...)
			}
			if ch := dec.AudioInfo().Channels; ch > 0 {
				return len(pcm) / 2 / ch, first, pcm
			}
			return 0, first, pcm
		}
		n1, start, _ := pass()
		if err := dec.Seek(start); err != nil {
			t.Fatalf("%s: Seek(%v) after the end: %v", c.path, start, err)
		}
		n2, first, pcm := pass()
		dec.Close()
		t.Logf("%s: %d samples, then %d after Seek(%.3f) at the end (first frame at %.3f s)", c.path, n1, n2, start, first)
		if d := n2 - n1; first < 0 || d < -1024 || d > 4224 {
			t.Errorf("%s: %d samples after Seek(%.3f) at the end, %d before it", c.path, n2, start, n1)
		} else if r := rms(pcm); r < 100 {
			t.Errorf("%s: the audio after Seek(%.3f) at the end is silence (RMS %.1f)", c.path, start, r)
		}
	}
}

// An AAC stream that opens and never decodes ends in an error, not in silence
// (#95). testdata/aac/zeroed.m4a is lc.m4a with every byte of its audio data
// (the mdat box) set to zero, so the header describes AAC-LC stereo and no
// packet decodes; zeroed_short.m4a is its first 0.5 s (ffmpeg -t 0.5 -c copy),
// 24 packets. FFmpeg's aac refuses the first packet as invalid data. Android's
// and Apple's decoders take every packet and return nothing, so the bridge
// gives up after
// MEDIACODEC_AUDIO_NO_FRAME_PACKETS packets (zeroed.m4a's 132), or at the end
// of the input (zeroed_short.m4a's 24), with an error wrapping ErrNoDecoder
// that names the decoder: the phones have no other AAC decoder (#77). Before
// #95 the audio there ended cleanly, having played nothing.
func TestAACNoFrame(t *testing.T) {
	aacTestClips(t)
	for _, path := range []string{"testdata/aac/zeroed.m4a", "testdata/aac/zeroed_short.m4a"} {
		start := time.Now()
		dec, err := Open(path)
		if err != nil {
			t.Fatalf("%s: Open: %v", path, err)
		}
		decoder := dec.(interface{ audioDecoder() string }).audioDecoder()
		audio := 0
		for {
			var mt MediaType
			mt, err = dec.Next()
			if err != nil || mt == MediaTypeNone {
				break
			}
			if mt == MediaTypeAudio {
				if b, _ := dec.GetAudio(); len(b) > 0 {
					audio++
				}
			}
		}
		took := time.Since(start)
		dec.Close()
		t.Logf("%s: %s: %d audio frames, then %v, in %v", path, decoder, audio, err, took.Round(time.Millisecond))
		if audio != 0 {
			t.Errorf("%s: %d audio frames with samples from data that is all zeros", path, audio)
		}
		if err == nil {
			t.Errorf("%s: Next reached the end with no error, and no audio", path)
			continue
		}
		if want := aacDecoder(); want != "aac" {
			if decoder != want || !errors.Is(err, ErrNoDecoder) || !strings.Contains(err.Error(), want) {
				t.Errorf("%s: %s, Next: %v; want %s, and an error wrapping ErrNoDecoder that names it", path, decoder, err, want)
			}
		} else if !errors.Is(err, avErrorInvalidData) {
			t.Errorf("%s: Next: %v; want FFmpeg's aac refusing the data as invalid", path, err)
		}
		if took > 10*time.Second {
			t.Errorf("%s: the error took %v", path, took)
		}
	}
}
