# Building goffmpeg

goffmpeg loads a purpose-built FFmpeg as shared libraries (Player ADR-0037 D3,
#40). The libraries are distributed as per-platform companion modules; cgo
selects the right one through the build-tagged files (`libav_darwin_arm64.go`
and friends).

## Where the artifacts come from

Nothing needs doing on macOS and Linux: `go build` resolves them, and `go run`
and `go test` find them in the module cache through an rpath. Windows needs one
step, under [Linking](#linking). The libraries are published as one module per
platform under
[goffmpeg-libs](https://github.com/belluikit/goffmpeg-libs), and each
`libav_<target>.go` blank-imports the matching one under a build constraint:

```go
//go:build darwin && arm64 && !ios

import _ "github.com/belluikit/goffmpeg-libs/darwin_arm64"
```

The blank import contributes that platform's `#cgo LDFLAGS`, which propagate to
the final link. `make sync-libs` writes them (see [Linking](#linking)). Because each platform is a separate module, a build downloads
only its own — around 20-30MB rather than the ~245MB all eleven would cost.

The FFmpeg *headers* are identical for every target, so one copy lives here in
`internal/libav/include` and is shared. Only `ffversion.h` ever differs, and
only in a cosmetic version string.

## Rebuilding a target

Needed when bumping FFmpeg or changing its configuration:

```sh
git clone https://github.com/FFmpeg/FFmpeg.git ../ffmpeg
git -C ../ffmpeg checkout --detach e2bc6d88cd68184288c16570e04881c3a426a05a
make dist TARGET=linux_amd64            # the media codec set, into shared/
make dist TARGET=linux_amd64 SET=audio  # the audio codec set, into shared-audio/
make sync-libs TARGET=linux_amd64       # copy both into ../goffmpeg-libs, write its link files
```

`make dist` alone builds the host target; `make dist-all` builds everything this
machine can produce without a container. `make sync-headers` refreshes the
shared headers after an FFmpeg bump.

`sync-libs` first runs `make check-paths`, which refuses libraries that name a
personal home directory. FFmpeg records its configure line inside the libraries,
toolchain paths included, and the linker records a dylib's output path, so a
build made under `/Users/<you>` publishes that name. FFmpeg is configured with
the neutral prefix `/<target>` for this reason. A local Android build still
fails the check, because the NDK path is in the configure line. Publish
libraries from CI, whose GitHub-hosted runners build as the generic `runner`
user.

Then tag the companion module — nested modules take a `<subdir>/vX.Y.Z` tag —
and bump the requirement in this module's `go.mod`:

```sh
cd ../goffmpeg-libs && git tag linux_amd64/v0.2.0 && git push origin --tags
```

## Shared libraries

Apps built on Player ship FFmpeg as shared libraries, so a user can replace
them (LGPL 2.1 §6b; Player ADR-0037 D3, goffmpeg-libs#14). They are the only
FFmpeg goffmpeg builds: the static archives were last published in
goffmpeg-libs v0.4.0 (#45). Each target is built twice, once per codec set
([What is in the build](#what-is-in-the-build)): `make dist` builds the media
set into `dist/<target>/shared`, `make dist SET=audio` the audio set into
`dist/<target>/shared-audio`, and `sync-libs` copies both into the companion
module under the same names:

```sh
make dist TARGET=linux_amd64 && make dist TARGET=linux_amd64 SET=audio
make smoke-shared TARGET=linux_amd64            # decode an MP4's video through the media set
make smoke-shared TARGET=linux_amd64 SET=audio  # an MP3 through the audio set
make sync-libs TARGET=linux_amd64
```

The configure line is the base line with `--enable-shared --disable-static`
and the flags below, and each `NOTICE` records it.

Every name carries the suffix `-goffmpeg` (`--build-suffix`), so it cannot be
mistaken for another FFmpeg in the same process or package: a distro
`libavcodec.so.62`, an `avcodec-62.dll` another component loaded, another SDK's
`libavcodec.so` in an APK. FFmpeg's checklist (ffmpeg.org/legal.html) allows
it: "Do not rename FFmpeg dlls to some obfuscated name, but adding a suffix or
prefix is fine".

| Target | Files in `shared/` | How the loader finds them |
|---|---|---|
| `darwin_*` | `libavcodec-goffmpeg.63.dylib`, install name `@rpath/libavcodec-goffmpeg.63.dylib` | the executable's rpath |
| `ios_arm64`, `iossimulator_arm64` | `libavcodec-goffmpeg.framework`, install name `@rpath/libavcodec-goffmpeg.framework/libavcodec-goffmpeg`, `Info.plist`, ad-hoc signed | `@executable_path/Frameworks`, where the app embeds and re-signs them |
| `linux_*` | `libavcodec-goffmpeg-63.so`, soname the same | the executable's rpath |
| `android_*` | `libavcodec-goffmpeg.so`, soname the same, 16KB-aligned | the APK's `lib/<abi>/` |
| `windows_*` | `avcodec-goffmpeg-63.dll` and its import library `libavcodec-goffmpeg.dll.a` | the executable's directory |

The same goes for avutil, swresample, swscale, avformat and (desktop only)
avdevice. Each file is named the way its loader looks for it, and only that
name is kept. FFmpeg installs the other names as symlinks, and a Go module zip
leaves symlinks out. Android's soname has no version because an APK installs
only `lib*.so`.

The libraries carry no rpath of their own: whatever loads them supplies it.

### Linking

Both backends load these libraries (#40). A goffmpeg-libs module's `lib/`
holds only the purego bridge, on desktop.

`make sync-libs` writes each goffmpeg-libs module's link files from the files
in its `shared/` and `shared-audio/` (`scripts/gen-libav.sh`), so the version
numbers in the names are never typed by hand and an FFmpeg bump needs no edit:

- `libav.go` links the media set from `shared/`, under `!goffmpeg_novideo`.
- `libav_audio.go` links the audio set from `shared-audio/`, under
  `goffmpeg_novideo`.

A binary links exactly one set, so it lists only that set's libraries, and a
package carries only those. A linker records a library linked by path even
when nothing calls it, so a binary that named a library its package left out
would not load: that is why the sets are linked whole, rather than libswscale
being left out of one link. (goffmpeg-libs v0.4.0 and v0.5.0 had one set and
a `libav_swscale.go` instead.)

Each library is linked by its path, because a Go module zip leaves out the
`libavcodec.dylib`- or `libavcodec.so`-style symlinks that `-lavcodec` would
need. cgo accepts a path that ends in `.so` but not one that ends in `.so.63`.
So the Linux libraries carry their major version before the suffix,
`libavcodec-goffmpeg-63.so`, the way FFmpeg names its Windows DLLs. The
Makefile sets this through FFmpeg's own naming variables (`SHARED_MAKEVARS`),
so the soname and the other libraries' NEEDED entries agree with the file
name. goffmpeg-libs v0.6.0 and v0.7.0 kept FFmpeg's `.so.63` names and
linked them with `-l:libavcodec-goffmpeg.so.63`. Go 1.27.4 and 1.26.11 refuse
that flag (#59), so those releases do not build for Linux with cgo on a
current Go.

| Target | Running from source (`go run`, `go test`) | A packaged app |
|---|---|---|
| `darwin_*`, `linux_*` | an rpath to the module's `shared/` (`shared-audio/` with `goffmpeg_novideo`), in the module cache | the packager copies the libraries and replaces that rpath with its own (`@executable_path/../Frameworks`, `$ORIGIN`): the module's rpath names the build machine's module cache |
| `ios_*` | not run | the app embeds the frameworks and links with `-Wl,-rpath,@executable_path/Frameworks`. cgo refuses an rpath that starts with `@`, so the module cannot supply it |
| `android_*` | not run | the APK's `lib/<abi>/` |
| `windows_*` | **`shared/` on `PATH`**, see below | the DLLs beside the executable |

Windows has no rpath, so a binary that `go test` or `go run` builds in a
temporary directory does not find the DLLs in the module cache. Put the
module's `shared/` on `PATH` first, or `shared-audio/` for a `goffmpeg_novideo`
build (the two sets' DLLs are named apart, so both can be there at once):

```powershell
$env:PATH = "$(go list -m -f '{{.Dir}}' github.com/belluikit/goffmpeg-libs/windows_amd64)\shared;$env:PATH"
```

CI does the same in its Windows test legs.

A package should copy the libraries its binary lists (`otool -L`, `readelf -d`,
`objdump -p`), from the set's directory: an audio-only binary lists the
`-goffmpeg-audio` libraries of `shared-audio/` and nothing of `shared/`. `scripts/check-links.sh` checks a built binary the same way. CI runs
it on every target it links, with and without `goffmpeg_novideo`, and
`TestLinksSharedFFmpeg` checks it from inside wherever the tests run.

`make dist` ends with `make check-shared`, which reads every library the way
its loader will. It checks that the library's own name is its file name, and
that everything it links is beside it or part of the OS. On Android it also
refuses text relocations and 4KB alignment, on Apple a signature that does not
verify, and on Windows a DLL import Windows does not ship. `make smoke-shared`
links `testdata/sharedsmoke/smoke.c` against the shared libraries alone and,
where the machine can run the target, decodes `testdata/sample.mp4` with it.
CI runs it on macOS (arm64 natively, x86_64 under Rosetta), the iOS simulator,
both Linux targets and both Windows targets; the iOS device and Android slices
only link.

## Releasing after a bridge change

**Any change to `bridge.c` or `bridge.h` needs a companion release, not just a
tag here.** The cgo backend compiles the bridge from source and picks the change
up immediately; purego does not — it dlopens the prebuilt `libgoffmpeg` the
companion modules ship, so until those are re-released, a new entry point is
simply absent on that backend. This has now happened twice, to device
enumeration and to per-open options, and both times it was invisible on the cgo
side where the work was done.

The symptom is deliberately not a panic. Each new entry point is probed with
`Dlsym` and, when missing, reported as its own sentinel —
`ErrDeviceListUnavailable`, `ErrOpenOptionsUnavailable` — so an out-of-date
library says so instead of taking the process down. Seeing one of those in a
purego build means this section was skipped.

The matrix builds every artifact needed, including the shared bridge for all six
desktop targets, so a release is assembled from CI rather than by hand:

```sh
gh workflow run build.yml            # or push a change to bridge.c
gh run download <run-id>             # libav-<target> for each target
```

For each target, drop the downloaded `lib/` and `shared/` into the companion checkout, then
tag. The tags are per-module and must all move together — a mixed set means one
platform loads a library built from different sources than the rest:

```sh
cd ../goffmpeg-libs
git add . && git commit -m "feat: <what the bridge gained>"
for t in darwin_arm64 darwin_amd64 linux_amd64 linux_arm64 \
         windows_amd64 windows_arm64 android_arm64 android_arm \
         android_amd64 ios_arm64 iossimulator_arm64; do
  git tag "$t/vX.Y.Z"
done
git push origin main --tags
```

Then bump every `goffmpeg-libs/*` requirement in this module's `go.mod` to the
new version and tag this module. The macOS purego CI leg stages the *published*
library on purpose, so it is what confirms the release actually took: the tests
that were skipping on `Err*Unavailable` should start running.

## Target matrix

| TARGET | Toolchain required | Notes |
|---|---|---|
| `darwin_arm64` | Xcode CLT | Committed. VideoToolbox + avfoundation capture |
| `darwin_amd64` | Xcode CLT, nasm | Cross-compiled from arm64 via `clang -arch x86_64` |
| `ios_arm64` | Xcode iPhoneOS SDK | No avdevice; VideoToolbox hardware decode |
| `iossimulator_arm64` | Xcode iPhoneSimulator SDK | Simulator slice |
| `android_arm64` | Android NDK r26+ | MediaCodec via JNI, no TLS |
| `android_arm` | Android NDK r26+ | armeabi-v7a, no TLS |
| `android_amd64` | Android NDK r26+ | Emulator, no TLS |
| `linux_amd64` | Docker (`make linux-docker`) | OpenSSL for https, v4l2 capture; glibc 2.35 and up |
| `linux_arm64` | Docker (`make linux-docker`) | OpenSSL for https, v4l2 capture; glibc 2.35 and up |
| `windows_amd64` | mingw-w64 | Schannel for TLS, dshow capture |
| `windows_arm64` | llvm-mingw | Schannel for TLS, dshow capture; distro mingw-w64 is x86 only |

Build the Linux targets with `make linux-docker`, which runs the build in a
Ubuntu 22.04 container:

```sh
make linux-docker TARGET=linux_arm64 DOCKER_GOALS="dist smoke-shared bridge-shared SMOKE_RUN=1"
```

The container sets the oldest glibc goffmpeg supports: **2.35** (Ubuntu 22.04
LTS, Debian 12 and later; #44). A shared library records the glibc symbol
versions it was linked against, so it needs at least the glibc it was built
with, whatever the system it runs on has. A build on the host's own, newer
glibc would raise the floor without anyone choosing to. `check-shared` and
`bridge-shared` fail a Linux file that needs more than `GLIBC_FLOOR`
(`scripts/check-glibc.sh`), and CI runs goffmpeg's Linux tests on Ubuntu 22.04
(`scripts/linux-floor.sh`).

### The iOS simulator

Go exposes no separate `GOOS` for the simulator, so the two slices are selected
by a build tag. Linking the device libraries into a simulator binary fails with
a platform mismatch, so the tag is required:

```sh
SIM=$(xcrun --sdk iphonesimulator --show-sdk-path)
CGO_ENABLED=1 GOOS=ios GOARCH=arm64 \
  CC="$(xcrun -f clang) -arch arm64 -isysroot $SIM -mios-simulator-version-min=13.0" \
  go build -tags iossimulator ./...
```

## A note on CGO_CFLAGS / CGO_LDFLAGS

If your shell exports these — Homebrew users often do —

    CGO_CFLAGS=-I/opt/homebrew/include
    CGO_LDFLAGS=-L/opt/homebrew/lib

they leak into every cgo build, **including cross-compiles**. That injects
host-only linker flags into non-host links and, worse, puts a Homebrew FFmpeg
ahead of ours on the search path, so a cross build can silently compile against
the wrong headers. Unset them when building or link-checking a non-host target:

    env -u CGO_CFLAGS -u CGO_LDFLAGS go test -c -o /dev/null .

## Verifying a target links

`go build` only compiles a library package — it never runs the linker, so a
missing system library is invisible until something links a binary. `go test -c`
produces one:

    CGO_ENABLED=1 GOOS=android GOARCH=arm64 CC=<ndk>/aarch64-linux-android21-clang \
      env -u CGO_CFLAGS -u CGO_LDFLAGS go test -c -o /dev/null .

CI runs this for every cross target, then `scripts/check-links.sh` on the
binary, and the real suite on the targets whose runner is the target platform:
darwin_arm64, linux_amd64, linux_arm64 and both Windows targets.

## What is in the build

A **decode-only** FFmpeg in two **codec sets** (#41). Encoders, muxers and
avfilter are off in both: this library only ever decodes.

| Set | Targets | Decoders | Demuxers | Where |
|---|---|---|---|---|
| **media** | desktop: `darwin_*`, `linux_*`, `windows_*` | every one FFmpeg has (478) | every one (360) | `shared/`, `-goffmpeg` |
| **media** | phones: `android_*`, `ios_*` | `mp3float`, `pcm_s16le` `s24le` `s32le` `f32le` `f64le` `u8`, `h264`, `aac` | `mp3`, `wav`, `mov`; `hls`, `mpegts` and what `hls` selects: `aac`, `ac3`, `eac3` | `shared/`, `-goffmpeg` |
| **audio** | every target | `mp3float`, the same PCM | `mp3`, `wav` | `shared-audio/`, `-goffmpeg-audio` |

On the phones the media set decodes only what apps play, so an app carries no
codec it doesn't need, and none of the patent-encumbered ones unless it plays
them. Desktop keeps every codec (the owner's choice on #41). The audio set has
no libswscale and no libavdevice, on every target; goffmpeg links it under the
`goffmpeg_novideo` tag.

On the phones, the media set enables VideoToolbox's H.264 hwaccel (iOS) and no
other, has the parsers its formats need (`mpegaudio`, `h264`, `aac`, and `ac3`,
which `hls` selects) and no bitstream filter that isn't selected by something
it keeps. On desktop, the media set has every hwaccel, parser and filter, and
one input device, the one that reads cameras on each OS (`avfoundation`,
`v4l2`, `dshow`): the rest aren't used. Every protocol stays, so `http`, and
`https` where the target has TLS, still open.

On android_arm64 the libraries come to 4.7 MB for the media set and 1.7 MB for
the audio set, against 14.4 MB for an every-codec build.

**The sets are enforced, not only configured.** `configure` turns on whatever
an enabled component selects, so a flag that asks for one decoder can bring
others: VideoToolbox's HEVC, AV1 and VP9 hwaccels brought their decoders back,
and the `eac3_core` and `dts2pts` filters the AC-3 and HEVC parsers. So every
trimmed build starts with decoders, demuxers, parsers, bitstream filters,
hwaccels and input devices off, and `make dist` runs `make check-components`
(`scripts/check-components.sh`), which fails the build when
`config_components.h` enables anything but the set's lists. A desktop media
build is held to having at least the phone media set's components and at least
300 decoders, and exactly its one input device. At run time,
`TestBuiltCodecsAreTheSet` lists the decoders and demuxers the loaded FFmpeg
holds and checks them the same way.

An input in a container a set leaves out fails to open with
`ErrUnrecognizedInput`. A stream whose codec it leaves out is named in
`MissingDecoder`, and an input with nothing it decodes fails with
`ErrNoDecoder` (README, "When a build can't decode something").

`--disable-autodetect` is deliberate. It stops `configure` from linking
whatever happens to be installed on the build machine, which would make the
artifacts non-portable. Everything enabled is either internal to FFmpeg or part
of the host OS, so the result is self-contained.

There is no `--enable-gpl` and no `--enable-nonfree`: the build is **LGPL 2.1+**.
Keep it that way — adding a GPL component (x264, x265) would relicense the
whole artifact and everything linking it.

## Bumping FFmpeg

The revision is pinned in two places that must stay in step:

- `FFMPEG_REF` in `.github/workflows/build.yml`
- the `checkout` line in this file

After bumping, rebuild every target — a partial matrix mixing FFmpeg versions
across platforms is not supported, and re-check `patches/` below: `make dist`
stops with an error if one no longer applies, which is the moment to rebase it
or drop it because upstream fixed it.

Before publishing libraries built from the new revision, tag it in
[belluikit/ffmpeg](https://github.com/belluikit/ffmpeg), our mirror of FFmpeg,
and point every companion `NOTICE` at that tag. The libraries are LGPL-2.1,
which asks for the corresponding source to be offered from the same place as
the binaries, so a NOTICE linking only upstream depends on a repository we do
not control. Old tags stay as long as anything built from them is published:

```sh
git -C ../ffmpeg push https://github.com/belluikit/ffmpeg.git \
  <sha>:refs/tags/goffmpeg-<short sha>
```

A non-empty `patches/` is part of that source too: commit the patches onto the
revision in the mirror and tag that commit instead.

## Local FFmpeg patches

`patches/*.patch` are applied to the pinned checkout by `make dist`, in
filename order. The directory should stay as close to empty as possible —
every entry is something to reconcile at the next FFmpeg bump — but waiting for
an upstream release to ship a fix we have already written is worse.

Applying is idempotent (`dist-all` configures seven targets from one source
tree, so it runs repeatedly against an already-patched checkout) and it fails
loudly rather than silently skipping when a patch no longer fits.

Each file carries a full commit message explaining the bug, so it can be sent
upstream as-is with `git format-patch`.

**The directory is currently empty**, which is where it should be. Its one
occupant was a v4l2 `mmap_free` fix for #14, dropped at this FFmpeg bump
because upstream `603234f945` had already made the same correction. That is
the intended lifecycle: carry a fix only until the pin catches up.

Before sending any patch here upstream, check whether it is already fixed on
FFmpeg master. This one was, three months before we hit it, and the duplicate
was one command away from going to a public mailing list:

```sh
git -C ../ffmpeg fetch origin master
git -C ../ffmpeg log --oneline -L '/^static void the_function/,/^}/:path/to/file.c' origin/master
```

To add one, edit the FFmpeg checkout, then:

```sh
git -C ../ffmpeg diff > patches/000N-short-description.patch
```

and put the explanation above the diff — `patch` ignores everything before the
first `---`, so the file is both a working patch and its own rationale.

## cgo and purego

The cgo backend compiles the bridge into the binary and loads FFmpeg's shared
libraries (see [Linking](#linking)). It is the supported path, and the only one
with `OpenReader` (custom AVIO), because feeding libavformat from an
`io.Reader` requires C calling back into Go.

The purego backend (`CGO_ENABLED=0`) dlopens `libgoffmpeg.{so,dll,dylib}`,
which every desktop companion module ships in `lib/`. Build one with
`make bridge-shared TARGET=<target>`. It loads the same shared FFmpeg
libraries, so a purego app ships it beside them (#40). On macOS and Linux it
looks for them beside itself and in `../shared`, which is where they are in
`dist/<target>` and in a goffmpeg-libs module. On Windows they must be beside
the executable or on `PATH`.

The bridge exports FFmpeg's log level as `decoder_log_set_level` and
`decoder_log_get_level`. purego looks every symbol up in libgoffmpeg's handle,
and on Windows that finds only what the DLL itself exports, not FFmpeg's
`av_log_set_level`, which it imports.

**`-static-libgcc`** for windows_amd64 matters, learned the hard way: mingw-w64
GCC otherwise links its runtime as `libgcc_s_seh-1.dll`, which Windows does not
ship, so the DLL loads only where MinGW is on `PATH`. CI's Windows runners have
it, which is why the build checks the DLL's imports against what Windows ships,
plus the FFmpeg DLLs beside it.

**`--enable-pic`** stays in the base configure line: `--extra-cflags=-fPIC`
covers the C only, and FFmpeg's hand-written assembly needs FFmpeg's own flag
to be position-independent.

## Network sources

The Android libraries have no TLS, so `https` URLs fail to open there with an
`AVError` of `AVERROR_PROTOCOL_NOT_FOUND` ("protocol not found"). Plain `http` works, and `OpenReader` can take
`https` read through Go, but not HLS, because FFmpeg has to fetch the segments
itself.

`Open` takes a path or a URL, and for http, https and HLS the defaults are not
enough on a real network. `OpenWithOptions` passes a comma-separated
`"key=value"` string through to libavformat — the same shape `OpenDevice`
takes, and anything FFmpeg accepts works without this package knowing about it:

```go
dec, err := goffmpeg.OpenWithOptions(url,
    "rw_timeout=5000000,reconnect=1,reconnect_streamed=1,reconnect_delay_max=5")
```

| Option | Without it |
|---|---|
| `rw_timeout` | a stalled server hangs the open indefinitely |
| `reconnect`, `reconnect_streamed`, `reconnect_delay_max` | any dropped connection ends playback |
| `user_agent` | some CDNs reject the default |
| `headers` | no cookies, no auth, no referer |
| `http_persistent` | HLS reopens a connection per segment |
| `analyzeduration`, `probesize` | slow or failed probing on sparse streams |

The first two are what separate a working demo from unattended playback.

An option the input does not recognise is ignored rather than rejected, so one
string can be handed to a URL and a local path alike; only a token that cannot
be split into key and value is an error. `OpenReaderWithOptions` is the same
for custom IO, where `analyzeduration` and `probesize` matter most — a
forward-only reader is exactly where probing struggles.

Under `CGO_ENABLED=0` this needs a `libgoffmpeg` exporting `decoder_open_opts`.
An older one returns `ErrOpenOptionsUnavailable` rather than silently dropping
the options, and still serves `Open` normally.

## Camera capture

`OpenDevice` selects an input device per platform:

| Platform | format | url |
|---|---|---|
| macOS | `avfoundation` | device index, e.g. `"0"` |
| Linux | `v4l2` | device node, e.g. `"/dev/video0"` |
| Windows | `dshow` | **friendly name**, e.g. `"video=OBS Virtual Camera"` |

dshow has no positional form — it selects by name only, so the `video=` prefix
and the exact device name are both required.

`ListInputSources(format)` enumerates them programmatically — the API form of
`ffmpeg -f dshow -list_devices true -i dummy`:

```go
devs, err := goffmpeg.ListInputSources("dshow")   // Name, Description, MediaTypes
dec, err := goffmpeg.OpenDevice("dshow", "video="+devs[0].Name, "")
```

Per-format behaviour differs, and callers have to account for all three:

- **v4l2** reports each `/dev/videoN` with the card name as its description,
  and no media types of its own — so the bridge queries each node's
  `V4L2_CAP_*` directly to fill them in. **Filter on `HasMedia` before
  opening.** Several nodes commonly belong to one camera, and the extra one
  carries metadata: opening it does not fail, it *blocks indefinitely* inside
  `avformat_open_input`. The capability query is what tells them apart (via
  `device_caps`, not `capabilities` — the latter is the union across the whole
  physical device and claims video capture on every node of it).
- **dshow** reports video *and* audio sources, with media types filled in, so
  filtering is required. `Name` is the DirectShow unique name (the
  `@device_pnp_…` form) which stays stable across identical cameras;
  `Description` is the friendly name. Either opens, prefixed with `video=`.
- **avfoundation** implements no enumeration callback at all and returns
  `ErrDeviceListUnsupported`. macOS selection stays positional (`"0"`).

`DeviceInfo.MediaKnown` separates "determined to provide neither" from "could
not determine", and both directions matter: an undetermined device must still
be tried, or a working camera is dropped; a device determined to do no video
must be skipped, or a caller hangs on the metadata node above. `HasMedia`
encodes that — prefer it to reading `MediaTypes` directly.

The purego backend reaches these through the same prebuilt `libgoffmpeg` shared
library the companion modules ship, so enumeration needs a library built at or
after the release that introduced it. Against an older one the symbols are
absent, the bindings stay nil, and `ListInputSources` returns
`ErrDeviceListUnavailable` rather than panicking in `RegisterLibFunc` — a
distinct sentinel from `ErrDeviceListUnsupported`, since the remedy is to
rebuild the library rather than to fall back to positional selection.

Verified on real hardware under both backends: v4l2 on linux/arm64 and dshow on
windows/arm64, each opening a camera at 1920x1080@60.
