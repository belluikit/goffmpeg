# Building goffmpeg

goffmpeg links against a purpose-built FFmpeg. The static libraries are
distributed as per-platform companion modules; cgo selects the right one
through the build-tagged files (`libav_darwin_arm64.go` and friends).

## Where the artifacts come from

Nothing needs doing: `go build` resolves them. The static libraries are
published as one module per platform under
[goffmpeg-libs](https://github.com/belluikit/goffmpeg-libs), and each
`libav_<target>.go` blank-imports the matching one under a build constraint:

```go
//go:build darwin && arm64 && !ios

import _ "github.com/belluikit/goffmpeg-libs/darwin_arm64"
```

The blank import contributes that platform's `#cgo LDFLAGS`, which propagate to
the final link. Because each platform is a separate module, a build downloads
only its own — around 20-30MB rather than the ~245MB all eleven would cost.

The FFmpeg *headers* are identical for every target, so one copy lives here in
`internal/libav/include` and is shared. Only `ffversion.h` ever differs, and
only in a cosmetic version string.

## Rebuilding a target

Needed when bumping FFmpeg or changing its configuration:

```sh
git clone https://github.com/FFmpeg/FFmpeg.git ../ffmpeg
git -C ../ffmpeg checkout --detach e2bc6d88cd68184288c16570e04881c3a426a05a
make dist TARGET=linux_amd64        # build
make sync-libs TARGET=linux_amd64   # copy into ../goffmpeg-libs
```

`make dist` alone builds the host target; `make dist-all` builds everything this
machine can produce without a container. `make sync-headers` refreshes the
shared headers after an FFmpeg bump.

Then tag the companion module — nested modules take a `<subdir>/vX.Y.Z` tag —
and bump the requirement in this module's `go.mod`:

```sh
cd ../goffmpeg-libs && git tag linux_amd64/v0.2.0 && git push origin --tags
```

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

For each target, drop the downloaded `lib/` into the companion checkout, then
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
| `android_arm64` | Android NDK r26+ | MediaCodec via JNI |
| `android_arm` | Android NDK r26+ | armeabi-v7a |
| `android_amd64` | Android NDK r26+ | Emulator |
| `linux_amd64` | gcc, nasm, libssl-dev | OpenSSL for https, v4l2 capture |
| `linux_arm64` | gcc, nasm, libssl-dev | |
| `windows_amd64` | mingw-w64 | Schannel for TLS, dshow capture |
| `windows_arm64` | llvm-mingw | Distro mingw-w64 is x86 only |

`make linux-docker TARGET=linux_amd64` builds the Linux targets in a container
when the host has no cross toolchain.

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

CI runs this for every cross target, and the real suite on the targets whose
runner is the target platform.

## What is in the build

A broad **decode-only** FFmpeg: every native decoder, demuxer, parser and
protocol (484 decoders, 359 demuxers), with encoders, muxers and avfilter
disabled because this library only ever decodes. That keeps `libavcodec.a`
around 14MB rather than a stock build's bulk.

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

The cgo backend links the static libraries directly and is the supported path;
it is also the only one with `OpenReader` (custom AVIO), because feeding
libavformat from an `io.Reader` requires C calling back into Go.

The purego backend (`CGO_ENABLED=0`) dlopens `libgoffmpeg.{so,dll,dylib}`,
which every desktop companion module now ships. Build one with
`make bridge-shared TARGET=<target>`; it embeds the static FFmpeg libraries, so
the result is a single self-contained object.

Three flags matter for it, all learned the hard way:

- **`--enable-pic`** — `--extra-cflags=-fPIC` covers the C only. FFmpeg's
  hand-written assembly needs FFmpeg's own flag, or linking the static
  libraries into a shared object fails on x86_64 *and* aarch64.
- **`-Wl,-Bsymbolic`** on Linux — the assembly addresses global tables with
  page-relative relocations, which are only valid when the symbol cannot be
  interposed at load time. Without it the linker reports
  "may bind externally ... recompile with -fPIC", which is misleading: the
  objects are already PIC.
- **`-static-libgcc`** for windows_amd64 — mingw-w64 GCC otherwise links its
  runtime as `libgcc_s_seh-1.dll`, which Windows does not ship, so the DLL
  loads only where MinGW is on `PATH`. CI's Windows runners have it, which is
  why the build now checks the DLL's imports against what Windows ships.

## Network sources

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
