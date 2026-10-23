package goffmpeg

import (
	"errors"
	"fmt"
)

// audioRequest remembers what the caller asked of the audio resampler.
//
// The bridge's Decoder struct carries the output rate in sample_rate, which
// decoder_open sets to the stream's rate and decoder_set_audio_channels hands
// to swresample as the output rate. Setting the field and rebuilding through
// decoder_set_audio_channels therefore converts the rate without a new bridge
// function — which matters for the purego backend, whose prebuilt shared
// bridges (goffmpeg-libs) would otherwise all need rebuilding first.
type audioRequest struct {
	channels   int   // as passed to SetAudioChannels; 0 keeps the stream's layout
	nativeRate int32 // the stream's own rate, read before the first change
}

// setRate points the resampler's output at rate (or the stream's own rate when
// rate <= 0) and rebuilds it with the channel count last requested. outRate is
// the bridge's sample_rate field; rebuild calls decoder_set_audio_channels.
func (a *audioRequest) setRate(rate int, outRate *int32, rebuild func(channels int) int) error {
	if *outRate <= 0 {
		return errors.New("goffmpeg: no audio stream to set a sample rate on")
	}
	if a.nativeRate == 0 {
		a.nativeRate = *outRate
	}
	want := a.nativeRate
	if rate > 0 {
		want = int32(rate)
	}
	prev := *outRate
	*outRate = want
	if res := rebuild(a.channels); res < 0 {
		*outRate = prev
		// The failed rebuild freed the old resampler; put one back at the
		// previous rate so the decoder keeps working as it did.
		rebuild(a.channels)
		return fmt.Errorf("set audio sample rate to %d failed: %d", rate, res)
	}
	return nil
}
