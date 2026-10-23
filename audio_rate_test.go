//go:build cgo

package goffmpeg

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// writeToneWAV writes a mono S16 WAV of a sine at freq Hz, amplitude 0.5.
func writeToneWAV(t *testing.T, rate, freq, seconds int) string {
	t.Helper()
	n := rate * seconds
	data := make([]byte, 44+2*n)
	copy(data[0:], "RIFF")
	binary.LittleEndian.PutUint32(data[4:], uint32(36+2*n))
	copy(data[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:], 16)
	binary.LittleEndian.PutUint16(data[20:], 1) // PCM
	binary.LittleEndian.PutUint16(data[22:], 1) // mono
	binary.LittleEndian.PutUint32(data[24:], uint32(rate))
	binary.LittleEndian.PutUint32(data[28:], uint32(rate*2))
	binary.LittleEndian.PutUint16(data[32:], 2)
	binary.LittleEndian.PutUint16(data[34:], 16)
	copy(data[36:], "data")
	binary.LittleEndian.PutUint32(data[40:], uint32(2*n))
	for i := 0; i < n; i++ {
		v := 0.5 * math.Sin(2*math.Pi*float64(freq)*float64(i)/float64(rate))
		binary.LittleEndian.PutUint16(data[44+2*i:], uint16(int16(math.Round(v*32767))))
	}
	path := filepath.Join(t.TempDir(), "tone.wav")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// decodeMono decodes every audio packet into float samples of channel 0.
func decodeMono(t *testing.T, dec IDecoder) []float64 {
	t.Helper()
	ch := dec.AudioInfo().Channels
	var out []float64
	for {
		mt, err := dec.Next()
		if err != nil || mt == MediaTypeNone {
			break
		}
		if mt != MediaTypeAudio {
			continue
		}
		b, err := dec.GetAudio()
		if err != nil {
			t.Fatalf("GetAudio: %v", err)
		}
		for i := 0; i+2*ch <= len(b); i += 2 * ch {
			out = append(out, float64(int16(binary.LittleEndian.Uint16(b[i:])))/32767)
		}
	}
	return out
}

// fitTone fits a·sin + b·cos at freq over x and returns the tone's amplitude
// and the SINAD in dB (tone power over everything else).
func fitTone(x []float64, rate, freq float64) (amp, sinadDB float64) {
	var ss, sc, s2, c2, cs float64
	for i, v := range x {
		w := 2 * math.Pi * freq * float64(i) / rate
		s, c := math.Sin(w), math.Cos(w)
		ss += v * s
		sc += v * c
		s2 += s * s
		c2 += c * c
		cs += s * c
	}
	det := s2*c2 - cs*cs
	a := (ss*c2 - sc*cs) / det
	b := (sc*s2 - ss*cs) / det
	var sig, res float64
	for i, v := range x {
		w := 2 * math.Pi * freq * float64(i) / rate
		fit := a*math.Sin(w) + b*math.Cos(w)
		sig += fit * fit
		res += (v - fit) * (v - fit)
	}
	return math.Hypot(a, b), 10 * math.Log10(sig/res)
}

// TestSetAudioSampleRateConverts decodes 44.1 kHz tones at 48 kHz. The frame
// count must follow the rate ratio, and a 15 kHz tone must keep its level:
// the linear resampler this replaces in bellui lost 3.4 dB there and dropped
// frames at chunk boundaries (belluikit/bellui#582).
func TestSetAudioSampleRateConverts(t *testing.T) {
	const seconds = 2
	for _, freq := range []int{1000, 15000, 18000} {
		path := writeToneWAV(t, 44100, freq, seconds)
		dec, err := Open(path)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		// Mono out, so the level measures the rate conversion alone:
		// swresample spreads a mono source over stereo at -3 dB a channel.
		if err := dec.SetAudioSampleRate(48000); err != nil {
			t.Fatalf("SetAudioSampleRate: %v", err)
		}
		if got := dec.AudioInfo(); got.SampleRate != 48000 || got.Channels != 1 {
			t.Fatalf("AudioInfo = %+v, want 48000 Hz x 1", got)
		}
		x := decodeMono(t, dec)
		dec.Close()

		// swresample holds back its filter's delay at end of stream (it is
		// never flushed), so allow a few dozen frames short, never long.
		want := 48000 * seconds
		if len(x) > want || len(x) < want-64 {
			t.Errorf("%d Hz: decoded %d frames, want %d (-64..0)", freq, len(x), want)
		}
		mid := x[len(x)/4 : 3*len(x)/4]
		amp, sinad := fitTone(mid, 48000, float64(freq))
		gain := 20 * math.Log10(amp/0.5)
		t.Logf("%5d Hz: frames %d, gain %.3f dB, SINAD %.1f dB", freq, len(x), gain, sinad)
		if math.Abs(gain) > 0.1 {
			t.Errorf("%d Hz: gain %.2f dB, want within 0.1 dB", freq, gain)
		}
		if sinad < 60 {
			t.Errorf("%d Hz: SINAD %.1f dB, want >= 60", freq, sinad)
		}
	}
}

// TestSetAudioSampleRateRestores checks rate <= 0 returns to the stream's own
// rate and that the channel request survives a rate change and back.
func TestSetAudioSampleRateRestores(t *testing.T) {
	dec, err := Open(writeToneWAV(t, 44100, 1000, 1))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer dec.Close()
	if err := dec.SetAudioChannels(2); err != nil {
		t.Fatal(err)
	}
	if err := dec.SetAudioSampleRate(48000); err != nil {
		t.Fatal(err)
	}
	if err := dec.SetAudioSampleRate(0); err != nil {
		t.Fatal(err)
	}
	if got := dec.AudioInfo(); got.SampleRate != 44100 || got.Channels != 2 {
		t.Fatalf("AudioInfo = %+v, want 44100 Hz x 2", got)
	}
	if n := len(decodeMono(t, dec)); n != 44100 {
		t.Errorf("decoded %d frames at the native rate, want 44100", n)
	}
}
