# goffmpeg

Go bindings for decoding media with FFmpeg. Open a file, a URL, an
`io.Reader` or a capture device (camera or microphone), then pull decoded
frames: video as RGBA into an `image.RGBA`, audio as interleaved signed 16-bit
PCM.

You do not need FFmpeg installed. The FFmpeg libraries come prebuilt, one Go
module per platform, from
[goffmpeg-libs](https://github.com/belluikit/goffmpeg-libs), and `go build`
fetches the one for your target. The build is decode-only: no encoders, no
muxers and no avfilter (see [BUILDING.md](BUILDING.md)).

Bellui's [`x/mmedia`](https://github.com/belluikit/bellui/tree/main/x/mmedia)
module depends on it.

## Install

Requires Go 1.26.2 or later (see `go.mod`).

```sh
go get github.com/belluikit/goffmpeg
```

The default backend uses cgo, so you need a C toolchain for your target
(`CGO_ENABLED=1`). See [Backends](#backends) for the `CGO_ENABLED=0` path.

## Usage

Decode a file and count what comes out:

```go
package main

import (
	"errors"
	"fmt"
	"image"
	"log"
	"os"
	"time"

	"github.com/belluikit/goffmpeg"
)

func main() {
	dec, err := goffmpeg.Open(os.Args[1]) // a path or a URL
	if err != nil {
		log.Fatal(err)
	}
	defer dec.Close()

	vi := dec.VideoInfo()
	fmt.Printf("%dx%d @ %.2f fps, %.1fs\n", vi.Width, vi.Height, vi.Framerate, dec.Duration())

	frame := image.NewRGBA(image.Rect(0, 0, vi.Width, vi.Height))
	var videoFrames, audioBytes int
	for {
		mt, err := dec.Next()
		switch {
		case errors.Is(err, goffmpeg.ErrAgain): // no data yet (live sources)
			time.Sleep(5 * time.Millisecond)
			continue
		case err != nil:
			log.Fatal(err)
		}
		switch mt {
		case goffmpeg.MediaTypeVideo:
			if err := dec.GetRGBA(frame); err != nil {
				log.Fatal(err)
			}
			videoFrames++
		case goffmpeg.MediaTypeAudio:
			pcm, err := dec.GetAudio() // interleaved signed 16-bit samples
			if err != nil {
				log.Fatal(err)
			}
			audioBytes += len(pcm)
		case goffmpeg.MediaTypeNone:
			fmt.Println("video frames:", videoFrames, "audio bytes:", audioBytes)
			return
		}
	}
}
```

`Next` has four outcomes: a video frame, an audio frame, a clean end of stream
(`MediaTypeNone` with a nil error), or an error. `ErrAgain` is not a failure.
A live device returns it between frames, and `Next` does not block, so sleep
briefly before calling again.

## API overview

All openers return an `IDecoder` (`VideoInfo`, `AudioInfo`, `Position`,
`Duration`, `Seek`, `SetAudioChannels`, `SetAudioSampleRate`, `Next`, `GetRGBA`, `GetAudio`, `Interrupt`, `Close`).

Every decoder also implements `YCbCrDecoder`, asserted rather than part of
`IDecoder`. Its `GetYCbCr(*image.YCbCr)` copies a video frame's planes as
decoded, with no conversion, for a caller that converts to RGB on the GPU
(#64). It also returns the frame's colour matrix (BT.601, BT.709 or BT.2020) and
range, and says when the matrix is a guess from the frame's height because the
stream does not state it. It copies 8-bit 4:2:0 only, which is nearly all H.264,
HEVC and VP9, planar or as the phones' hardware decoders deliver it (`nv12`,
split into `Cb` and `Cr`). Any other format returns `ErrPixelFormat` naming it, and
`GetRGBA` still converts it. Under `CGO_ENABLED=0` it needs a libgoffmpeg
that exports `decoder_get_yuv`.

| Function | Opens |
|---|---|
| `Open(url)` | A file path or URL |
| `OpenWithOptions(url, options)` | The same, with FFmpeg demuxer/protocol options as `"key=value,key=value"` |
| `OpenReader(r)` / `OpenReaderWithOptions(r, options)` | An `io.Reader`, without staging it on disk. Seekable if `r` is an `io.Seeker`. cgo only |
| `OpenDevice(format, url, options)` | A capture device via libavdevice |
| `ListInputSources(format)` | Lists capture devices as `DeviceInfo` |
| `ListDeviceModes(format, url)` | Lists the modes a device accepts as `DeviceMode`; `DeviceMode.Options()` renders one as an `OpenDevice` options string |

For network sources, set at least a read timeout and reconnects, or a stalled
or dropped connection hangs or ends playback:

```go
dec, err := goffmpeg.OpenWithOptions(url,
	"rw_timeout=5000000,reconnect=1,reconnect_streamed=1,reconnect_delay_max=5")
```

To stop a decoder that is blocked on the network (a far seek in an MP3 with no
seek table reads the file through to the target), call `Interrupt` from
another goroutine before `Close`: the blocked `Seek` or `Next` returns an
error at FFmpeg's next interrupt check. A decoder from `OpenReader` stops at
its next call to the reader, so cancel a reader that can block yourself.

Capture devices per platform:

| Platform | `format` | `url` |
|---|---|---|
| macOS | `avfoundation` | device index, e.g. `"0"` |
| Linux | `v4l2` | device node, e.g. `"/dev/video0"` |
| Windows | `dshow` | `"video=<name>"`; names come from `ListInputSources("dshow")` |

`avfoundation` cannot enumerate devices and returns
`ErrDeviceListUnsupported`. On Linux, filter `ListInputSources` results with
`DeviceInfo.HasMedia(goffmpeg.MediaTypeVideo)` before opening: a camera's
metadata node blocks indefinitely when opened. The iOS and Android builds have
no libavdevice, so there is no device capture there.

FFmpeg's own log goes to stderr, and the package sets it to errors only, so
its warnings (a missing SIMD conversion path, the AAC decoder's discarded
priming samples after a seek) stay out of your application's output.
`SetLogLevel(goffmpeg.LogWarning)` or `LogInfo` brings them back while
debugging; the level is process-wide.

Decoder failures carry the FFmpeg error code as `AVError`, reachable with
`errors.As`. [BUILDING.md](BUILDING.md) has the full detail on network options,
capture and per-format behaviour.

## Platforms

One build-tagged file per target (`libav_<goos>_<goarch>.go`) imports the
matching goffmpeg-libs module:

| GOOS/GOARCH | Notes from the source |
|---|---|
| `darwin/arm64`, `darwin/amd64` | avfoundation capture; VideoToolbox H.264 decode with `GOFFMPEG_HWDECODE=1` (see below) |
| `linux/amd64`, `linux/arm64` | v4l2 capture; OpenSSL for https |
| `windows/amd64`, `windows/arm64` | dshow capture; Schannel for TLS |
| `ios/arm64` | No avdevice; H.264 decodes on VideoToolbox, and VP9 and AV1 where the device decodes them in hardware (see Hardware decoding) |
| `ios/arm64` with `-tags iossimulator` | The iOS simulator. The tag is required: Go has no separate GOOS for it |
| `android/arm64`, `android/arm`, `android/amd64` | No avdevice; H.264, VP9 and AV1 decode on MediaCodec (see Hardware decoding); **no TLS, so `https` URLs cannot open** |

If your shell exports `CGO_CFLAGS` or `CGO_LDFLAGS` (common with Homebrew),
unset them when cross-compiling. Otherwise a cross build can pick up host
headers or a Homebrew FFmpeg. See [BUILDING.md](BUILDING.md).

## Hardware decoding

On iOS, H.264 video decodes on VideoToolbox by default, and VP9 and AV1 do
where the device decodes them in hardware (an iPad of the 10th generation does
neither, and decodes them in software). On Android H.264, VP9 and AV1 decode on
MediaCodec (`h264_mediacodec`, `vp9_mediacodec`, `av1_mediacodec`) when the
video is larger than 1280×720, and in software at that size and below, where
MediaCodec costs more than it saves (measured below). Only 8-bit 4:2:0 streams
go to the hardware: H.264 Baseline, Main and High, VP9 profile 0, AV1 Main.
Anything else (High 4:4:4, 10-bit, other codecs)
decodes in software, and so does a stream the hardware decoder will not open.
So does a stream the device has no hardware decoder for, such as AV1 on most
phones without a flagship chip: Android would otherwise decode it with its own
software decoder in its media process, which costs more than goffmpeg's (#85).
That check needs the app's Java VM, so Android 12 or later; a process with no
Java VM uses whatever decoder Android offers, as before. If MediaCodec opens a stream and
then fails before its first frame, the software decoder takes over from the
start, with no frame lost. Frames come out the same either way: `GetRGBA`
converts them as before.

MediaCodec hands its frames back over binder, which needs a binder thread
pool in your process. An app always has one. In a process with no Java VM,
such as a test binary, a command-line tool or a daemon, goffmpeg starts one
(`ABinderProcess_startThreadPool`) the first time it opens MediaCodec. Without
it, on a Pixel 9a (Android 17) the decode never returned (#86).

On Android, AAC decodes with Android's own decoder, through FFmpeg's
`aac_mediacodec`, and the Android builds carry no FFmpeg AAC decoder (#77):
the platform's decoder comes with Google's licence for the format. It is a
software decoder in Android's media process (`c2.android.aac.decoder` on the
phones measured), so it is used at every size and `GOFFMPEG_HWDECODE` does not
change it. AAC-LC, HE-AAC and HE-AACv2 from MP4, ADTS and HLS come out at the
rate, channel count and length FFmpeg's own decoder gives them; on a OnePlus
Pad 2 and a Pixel 9a, AAC-LC was within 72–79 dB of it, and HE-AAC kept its
high band and HE-AACv2 its stereo. Opening an input with AAC costs more:
84–155 ms on the OnePlus and 76–215 ms on the Pixel, against 1–24 ms with
FFmpeg's decoder, because
FFmpeg's stream probing opens one MediaCodec decoder to learn the stream's
real format and playback opens a second. Three FFmpeg patches make it work
(`patches/0002`–`0004`, see BUILDING.md).

On iOS, AAC decodes with Apple's own decoder, through FFmpeg's `aac_at`
(AudioToolbox), and the iOS builds carry no FFmpeg AAC decoder either (#77).
FFmpeg's `aac_at` decoded only HE-AAC's core, at half the rate, and HE-AACv2 in
mono; `patches/0005` makes it decode every layer, signalled or implicit, so the
clips above come out at the rate, channel count and length FFmpeg's own
decoder gives them. One case differs: AAC-LC at 24 kHz or below that says
nothing about HE-AAC comes out at twice its rate, as from any decoder that
assumes HE-AAC there, where FFmpeg's decoder keeps the rate.

On desktop, video decodes in software unless you ask. The `GOFFMPEG_HWDECODE`
environment variable, read when each input is opened, controls this:

| `GOFFMPEG_HWDECODE` | |
|---|---|
| unset | hardware on iOS, and on Android above 1280×720; software elsewhere |
| `0` | software video everywhere (AAC on Android still decodes on Android's decoder) |
| `1` | as unset, and also VideoToolbox on macOS |
| `force` | the hardware wherever the bridge has it, without the profile and size checks (for tests) |

Measured in bellui's video player (#65, PR #70), process CPU in one core's
terms. On an OPD2403 phone (Snapdragon 8 Gen 3, Android 16), with MediaCodec's
own `media.hwcodec` process counted in: 1080p30 about 110% in software and
96% with MediaCodec; 720×720 about 71% in software and 77% with MediaCodec,
which also dropped a few frames where software dropped none. Hence the size
threshold. On an iPad (10th generation), VideoToolbox makes no measurable
difference to the player's CPU at either size, with the same frames, byte for
byte. On its own (no player), goffmpeg uses 10–17 points of a core less with
it, which counts once frames go to the GPU as YUV.

## Backends

- **cgo** (default, `CGO_ENABLED=1`): compiles the C bridge from source and
  loads FFmpeg's shared libraries from the goffmpeg-libs module. This is the
  supported path, and the only one with `OpenReader`.
- **purego** (`CGO_ENABLED=0`): loads the shared library `libgoffmpeg`
  (`.dylib`, `.so` or `.dll`) at startup. The desktop goffmpeg-libs modules
  ship it under `<target>/lib/`, but this backend does not import those
  modules. It looks for `internal/libav/dist/<goos>_<goarch>/lib/` under the
  current directory, then asks the system loader by name. So a deployed binary
  needs the library on the system search path. If it is not found, openers
  return `library not loaded`. Features newer than the loaded library report
  `ErrOpenOptionsUnavailable`, `ErrDeviceListUnavailable` or
  `ErrDeviceModesUnavailable` instead of crashing.

Either way FFmpeg is loaded as shared libraries, so that a user can replace
them (LGPL 2.1 §6b). An app ships them beside its binary. `go run` and `go
test` find them in the module cache on macOS and Linux; on Windows, put the
module's `shared/` on `PATH` first. See [BUILDING.md](BUILDING.md#linking).

## Formats

goffmpeg's FFmpeg comes in two codec sets (#41):

| Set | Plays | Linked by |
|---|---|---|
| **media**, desktop (macOS, Linux, Windows) | every format FFmpeg decodes; webcam capture | default |
| **media**, phones (Android, iOS) | MP3, WAV, MP4 (`.mp4`, `.m4a`, `.mov`), WebM, Ogg and HLS, with H.264, VP9 and AV1 video and AAC and Opus audio (AAC through Android's and Apple's own decoders) | default |
| **audio**, every platform | MP3, WAV, Opus (Ogg, `.opus`) and AAC (MP4 and `.m4a`, ADTS `.aac`); on the phones AAC through their own decoders | `-tags goffmpeg_novideo` |

VP9, AV1 and Opus are there for apps that move their content to royalty-free
formats (#78). AV1 decodes in software through dav1d, built into libavcodec on
every platform, desktop included: FFmpeg's own AV1 decoder only drives
hardware. On the phones, anything else (FLAC, Vorbis, HEVC and so on) isn't built,
and opening it says so ("When a build can't decode something", below). The
exact lists are in [BUILDING.md](BUILDING.md#what-is-in-the-build).

## Audio only

`-tags goffmpeg_novideo` builds the cgo backend against the audio set: MP3,
WAV, Opus and AAC, with no libswscale and no libavdevice, on every platform, so an
app ships 2.0 MB of FFmpeg where the phone media set is 6.6 MB (android_arm64). No video frame arrives,
`GetRGBA` returns `ErrVideoNotBuilt`, and there's no capture: `OpenDevice`
says the build has no libavdevice. An MP4 opens with its AAC, and its video is
left out and named in `VideoInfo().MissingDecoder` (#77). HLS fails to open
with `ErrUnrecognizedInput`. The tag doesn't change the purego backend, which loads
a prebuilt `libgoffmpeg` that links the media set.

## When a build can't decode something

A build decodes the codecs and reads the containers it was built with
([BUILDING.md](BUILDING.md#what-is-in-the-build)). An input opens with the
streams it can decode:

- **One stream decodes, the other doesn't:** the input opens, and
  `VideoInfo().MissingDecoder` or `AudioInfo().MissingDecoder` names the codec
  it couldn't decode (`"hevc"`). The AAC audio of an MP4 whose video the build
  can't decode still plays.
- **No stream decodes:** the open fails with `ErrNoDecoder`, naming the codecs.
- **The phone's AAC decoder opens and returns no audio:** Android and iOS
  decode AAC only with their own decoders, so there is nothing to fall back
  to. `Next` returns an error wrapping `ErrNoDecoder` that names the decoder
  (`aac_mediacodec`, `aac_at`). That happens when the decoder fails before
  its first frame, reaches the end of the input without one, or is sent 64
  packets (about 1.5 s of audio) and returns nothing, as both do for AAC data
  they can't decode. On desktop FFmpeg's `aac` refuses such data at once, as
  an `AVError`.
- **No demuxer recognizes the input,** or it isn't media: the open fails with
  `ErrUnrecognizedInput`, wrapping FFmpeg's `AVERROR_INVALIDDATA` as an
  `AVError`. Any other open failure wraps FFmpeg's own code too: a missing file
  is `AVError(-2)`, `ENOENT`.

Under `CGO_ENABLED=0` this needs a `libgoffmpeg` that exports
`decoder_missing` and `decoder_open_opts_err`; with an older one,
`MissingDecoder` reads empty and an open failure carries only the bridge's code.

## Building FFmpeg yourself

You only need this to bump FFmpeg or change its configuration.
[BUILDING.md](BUILDING.md) covers the pinned FFmpeg revision, `make dist`,
the target toolchains, local patches and the release process for
goffmpeg-libs.

## License

goffmpeg's own code (the Go, C and Objective-C files, the Makefile and the
build workflow) is BSD-3-Clause; see [LICENSE](LICENSE).

FFmpeg is not. The FFmpeg headers in `internal/libav/include/` are copied
unmodified from FFmpeg and remain under the GNU LGPL, version 2.1 or later
([COPYING.LGPLv2.1](COPYING.LGPLv2.1)). The prebuilt libraries in
[goffmpeg-libs](https://github.com/belluikit/goffmpeg-libs) are built without
`--enable-gpl` and `--enable-nonfree`, so they are LGPL 2.1 or later too. A
program that links them is a "work that uses the Library" under LGPL 2.1 and
must meet its section 6 conditions when distributed.

The exact FFmpeg revision, configure line and source location for each
platform's libraries are in the `NOTICE` file of that platform's goffmpeg-libs
module. This repository's [NOTICE](NOTICE) covers the headers.

Part of the [Bellui](https://github.com/belluikit/bellui) Go UI toolkit
ecosystem — <https://bellui.com>.
