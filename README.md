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
`Duration`, `Seek`, `SetAudioChannels`, `Next`, `GetRGBA`, `GetAudio`, `Close`).

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

Decoder failures carry the FFmpeg error code as `AVError`, reachable with
`errors.As`. [BUILDING.md](BUILDING.md) has the full detail on network options,
capture and per-format behaviour.

## Platforms

One build-tagged file per target (`libav_<goos>_<goarch>.go`) imports the
matching goffmpeg-libs module:

| GOOS/GOARCH | Notes from the source |
|---|---|
| `darwin/arm64`, `darwin/amd64` | avfoundation capture; VideoToolbox hardware decode on arm64 |
| `linux/amd64`, `linux/arm64` | v4l2 capture; OpenSSL for https on amd64 |
| `windows/amd64`, `windows/arm64` | dshow capture; Schannel for TLS on amd64 |
| `ios/arm64` | No avdevice; VideoToolbox hardware decode |
| `ios/arm64` with `-tags iossimulator` | The iOS simulator. The tag is required: Go has no separate GOOS for it |
| `android/arm64`, `android/arm`, `android/amd64` | No avdevice; MediaCodec hardware decode on arm64 |

If your shell exports `CGO_CFLAGS` or `CGO_LDFLAGS` (common with Homebrew),
unset them when cross-compiling. Otherwise a cross build can pick up host
headers or a Homebrew FFmpeg. See [BUILDING.md](BUILDING.md).

## Backends

- **cgo** (default, `CGO_ENABLED=1`): links FFmpeg's static libraries and
  compiles the C bridge from source. This is the supported path, and the only
  one with `OpenReader`.
- **purego** (`CGO_ENABLED=0`): loads the shared library `libgoffmpeg`
  (`.dylib`, `.so` or `.dll`) at startup. The desktop goffmpeg-libs modules
  ship it under `<target>/lib/`, but this backend does not import those
  modules. It looks for `internal/libav/dist/<goos>_<goarch>/lib/` under the
  current directory, then asks the system loader by name. So a deployed binary
  needs the library on the system search path. If it is not found, openers
  return `library not loaded`. Features newer than the loaded library report
  `ErrOpenOptionsUnavailable`, `ErrDeviceListUnavailable` or
  `ErrDeviceModesUnavailable` instead of crashing.

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
