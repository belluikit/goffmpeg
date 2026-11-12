FFMPEG_SRC ?= $(shell cd ../ffmpeg 2>/dev/null && pwd)
ROOT       := $(shell pwd)
DIST_ROOT  := $(ROOT)/internal/libav/dist
BUILD_ROOT := $(ROOT)/internal/libav/build
NPROC      := $(shell sysctl -n hw.ncpu 2>/dev/null || nproc)

# ---------------------------------------------------------------------------
# Target matrix
#
# Artifacts are laid out per platform as dist/<GOOS>_<GOARCH>/shared (the
# FFmpeg shared libraries) and, on desktop, dist/<GOOS>_<GOARCH>/lib (the purego
# bridge), so one checkout can hold every target at once.
#
# Build one with:  make dist TARGET=darwin_amd64
# Build the host:  make dist
# ---------------------------------------------------------------------------

HOST_ARCH := $(shell uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
HOST_OS   := $(shell uname -s | tr 'A-Z' 'a-z' | sed 's/^darwin$$/darwin/;s/^linux$$/linux/')
TARGET    ?= $(HOST_OS)_$(HOST_ARCH)

DIST_DIR  := $(DIST_ROOT)/$(TARGET)

# Common flags. See the note in FFMPEG_CONF below on why autodetect stays off.
#
# The prefix is neutral and `make install` adds DESTDIR, so the files still land
# in $(DIST_DIR). FFmpeg records its configure line in the libraries and reports
# it at runtime (avutil_configuration), and an absolute prefix put the build
# machine's home directory into every published library (goffmpeg-libs#6).
BASE_CONF = \
	--prefix="/$(TARGET)" \
	--extra-cflags="-fPIC" \
	--enable-pic \
	--disable-autodetect \
	--disable-programs \
	--disable-doc \
	--disable-debug \
	--disable-encoders \
	--disable-muxers \
	--disable-avfilter \
	--enable-swscale \
	--enable-swresample \
	--enable-network

# --- Apple ------------------------------------------------------------------
# VideoToolbox and SecureTransport ship with the OS, so they cost no external
# dependency. avdevice/avfoundation capture is desktop-only here; the mobile
# targets omit it to keep the mobile binaries lean.
MACOS_MIN  := 11.0
IOS_MIN    := 13.0
APPLE_CONF = --enable-securetransport --enable-videotoolbox

MACOS_CONF = $(APPLE_CONF) --enable-avdevice --enable-avfoundation

ifeq ($(TARGET),darwin_arm64)
	TARGET_CONF = --arch=arm64 --target-os=darwin $(MACOS_CONF) \
		--cc="clang -arch arm64 -mmacosx-version-min=$(MACOS_MIN)"
	LIB_EXT = dylib
endif

ifeq ($(TARGET),darwin_amd64)
	TARGET_CONF = --arch=x86_64 --target-os=darwin --enable-cross-compile $(MACOS_CONF) \
		--cc="clang -arch x86_64 -mmacosx-version-min=$(MACOS_MIN)"
	LIB_EXT = dylib
endif

ifeq ($(TARGET),ios_arm64)
	IOS_SDK := $(shell xcrun --sdk iphoneos --show-sdk-path)
	TARGET_CONF = --arch=arm64 --target-os=darwin --enable-cross-compile $(APPLE_CONF) \
		--disable-avdevice \
		--cc="clang -arch arm64 -isysroot $(IOS_SDK) -mios-version-min=$(IOS_MIN)" \
		--extra-ldflags="-isysroot $(IOS_SDK) -mios-version-min=$(IOS_MIN)"
	LIB_EXT = dylib
endif

ifeq ($(TARGET),iossimulator_arm64)
	SIM_SDK := $(shell xcrun --sdk iphonesimulator --show-sdk-path)
	TARGET_CONF = --arch=arm64 --target-os=darwin --enable-cross-compile $(APPLE_CONF) \
		--disable-avdevice \
		--cc="clang -arch arm64 -isysroot $(SIM_SDK) -mios-simulator-version-min=$(IOS_MIN)" \
		--extra-ldflags="-isysroot $(SIM_SDK) -mios-simulator-version-min=$(IOS_MIN)"
	LIB_EXT = dylib
endif

# --- Android ----------------------------------------------------------------
# The NDK ships a single clang that targets every ABI via the <triple><api>
# driver name. API 21 is the floor Go's own mobile support assumes.
ANDROID_NDK ?= $(firstword $(wildcard $(HOME)/Library/Android/sdk/ndk/*) $(ANDROID_NDK_HOME))
ANDROID_API ?= 21
NDK_BIN     := $(ANDROID_NDK)/toolchains/llvm/prebuilt/darwin-x86_64/bin

ifeq ($(TARGET),android_arm64)
	TARGET_CONF = --arch=aarch64 --target-os=android --enable-cross-compile \
		--disable-avdevice --enable-jni --enable-mediacodec \
		--cc="$(NDK_BIN)/aarch64-linux-android$(ANDROID_API)-clang" \
		--cross-prefix="$(NDK_BIN)/llvm-" --nm="$(NDK_BIN)/llvm-nm" --ar="$(NDK_BIN)/llvm-ar" \
		--ranlib="$(NDK_BIN)/llvm-ranlib" --strip="$(NDK_BIN)/llvm-strip"
	LIB_EXT = so
endif

ifeq ($(TARGET),android_arm)
	TARGET_CONF = --arch=arm --cpu=armv7-a --target-os=android --enable-cross-compile \
		--disable-avdevice --enable-jni --enable-mediacodec \
		--cc="$(NDK_BIN)/armv7a-linux-androideabi$(ANDROID_API)-clang" \
		--cross-prefix="$(NDK_BIN)/llvm-" --nm="$(NDK_BIN)/llvm-nm" --ar="$(NDK_BIN)/llvm-ar" \
		--ranlib="$(NDK_BIN)/llvm-ranlib" --strip="$(NDK_BIN)/llvm-strip"
	LIB_EXT = so
endif

ifeq ($(TARGET),android_amd64)
	TARGET_CONF = --arch=x86_64 --target-os=android --enable-cross-compile \
		--disable-avdevice --enable-jni --enable-mediacodec \
		--cc="$(NDK_BIN)/x86_64-linux-android$(ANDROID_API)-clang" \
		--cross-prefix="$(NDK_BIN)/llvm-" --nm="$(NDK_BIN)/llvm-nm" --ar="$(NDK_BIN)/llvm-ar" \
		--ranlib="$(NDK_BIN)/llvm-ranlib" --strip="$(NDK_BIN)/llvm-strip"
	LIB_EXT = so
endif

# --- Linux ------------------------------------------------------------------
# Built inside a container (see linux-docker) so the host needs no cross
# toolchain; these branches are what runs *inside* it, natively.
#
# The container also sets the oldest glibc goffmpeg supports (#44): a shared
# library needs at least the glibc it was built against, so the libraries are
# built on Ubuntu 22.04, whose glibc is the floor. check-shared and bridge-shared
# fail a Linux file that needs a newer one, which is what a build on a newer
# host produces.
GLIBC_FLOOR := 2.35
ifeq ($(TARGET),linux_amd64)
	TARGET_CONF = --arch=x86_64 --target-os=linux --enable-openssl --enable-avdevice
	LIB_EXT = so
endif

ifeq ($(TARGET),linux_arm64)
	TARGET_CONF = --arch=aarch64 --target-os=linux --enable-openssl --enable-avdevice
	LIB_EXT = so
endif

# --- Windows ----------------------------------------------------------------
ifeq ($(TARGET),windows_amd64)
	TARGET_CONF = --arch=x86_64 --target-os=mingw32 --enable-cross-compile \
		--cross-prefix=x86_64-w64-mingw32- --enable-schannel \
		--enable-avdevice
	LIB_EXT = dll
endif

# Windows on ARM needs llvm-mingw; the GNU mingw-w64 toolchain in the distro
# repositories only targets x86. Set LLVM_MINGW to an unpacked release from
# https://github.com/mstorsjo/llvm-mingw/releases.
LLVM_MINGW ?= /opt/llvm-mingw
ifeq ($(TARGET),windows_arm64)
	TARGET_CONF = --arch=aarch64 --target-os=mingw32 --enable-cross-compile \
		--cross-prefix=$(LLVM_MINGW)/bin/aarch64-w64-mingw32- --enable-schannel \
		--enable-avdevice
	LIB_EXT = dll
endif

ifndef TARGET_CONF
$(error unknown TARGET "$(TARGET)" — see the target matrix in this Makefile)
endif

# --- Codec sets (#41) --------------------------------------------------------
# Every target is built twice (the owner's choices on #41):
#
#   SET=media  On the phones (android_*, ios_*), only what apps play: MP3,
#              WAV, MP4 and HLS with H.264 video and AAC audio, so an app
#              carries no codec it doesn't need. On desktop (darwin_*,
#              linux_*, windows_*), every decoder, demuxer and parser FFmpeg
#              has. In shared/, named -goffmpeg as before.
#   SET=audio  MP3 and WAV only, on every target: no video decoder, no
#              libswscale, no libavdevice. In shared-audio/, named
#              -goffmpeg-audio, so a binary shows which set it loads and a
#              package can't mix the two. goffmpeg's goffmpeg_novideo tag
#              links it.
#
# In a trimmed set, decoders, demuxers and parsers start empty and only the
# set's are enabled. Protocols are untouched: http, and https where the target
# has TLS, still open. An input in a container left out fails with
# ErrUnrecognizedInput, and a stream whose codec is left out is named in
# MissingDecoder.
SET ?= media
comma := ,
empty :=
space := $(empty) $(empty)
# What each set decodes, as FFmpeg's component names. The configure flags are
# made from these lists, and check-components holds the configured build to
# them exactly.
SET_DEMUXERS = mp3 wav
SET_DECODERS = mp3float pcm_s16le pcm_s24le pcm_s32le pcm_f32le pcm_f64le pcm_u8
SET_PARSERS  = mpegaudio
SET_HWACCELS =
SET_INDEVS   =
ifeq ($(SET),media)
	SET_DEMUXERS += mov
	SET_DECODERS += h264 aac
	SET_PARSERS  += h264 aac
	# HLS (owner, #41), which bellui's x/mmedia plays: the playlist demuxer
	# and MPEG-TS for its segments. hls selects the aac, ac3 and eac3
	# demuxers and the ac3 parser, listed so check-components holds the set
	# to them. They read containers; no decoder comes with them.
	SET_DEMUXERS += hls mpegts aac ac3 eac3
	SET_PARSERS  += ac3
	SHARED_SUFFIX   := -goffmpeg
	SHARED_DIR_NAME := shared
else ifeq ($(SET),audio)
	SHARED_SUFFIX   := -goffmpeg-audio
	SHARED_DIR_NAME := shared-audio
else
$(error unknown SET "$(SET)": media or audio)
endif
# Webcam capture, desktop and media set only: the one input device that
# reads cameras on each OS, and the decoders for what cameras deliver, raw
# frames or MJPEG (v4l2, dshow). The other input devices are off: they aren't
# used, and gdigrab, Windows' screen capture, selects the bmp decoder.
ifeq ($(SET)$(filter darwin_% linux_% windows_%,$(TARGET)),media$(TARGET))
	SET_DECODERS += rawvideo mjpeg
	SET_INDEVS   += $(if $(filter darwin_%,$(TARGET)),avfoundation,$(if $(filter linux_%,$(TARGET)),v4l2,dshow))
endif
# VideoToolbox decodes through FFmpeg's own h264 decoder, which it selects.
ifeq ($(SET)$(filter darwin_% ios_% iossimulator_%,$(TARGET)),media$(TARGET))
	SET_HWACCELS += h264_videotoolbox
endif
# MediaCodec is a decoder of its own, not a hwaccel of h264 (goffmpeg-libs#21,
# #65). It selects the h264_mp4toannexb filter and the h264 parser, which the
# set already has. FFmpeg registers it after the software h264, so a lookup by
# codec ID still returns h264; a caller that wants the phone's decoder opens it
# by name.
ifeq ($(SET)$(filter android_%,$(TARGET)),media$(TARGET))
	SET_DECODERS += h264_mediacodec
endif

# The desktop media set has every codec (owner, #41): the lists above are
# then a floor rather than the set. check-components holds the build to having
# at least them and at least EVERY_MIN_DECODERS decoders, and to exactly its
# one camera input device.
EVERY_CODEC := $(if $(and $(filter media,$(SET)),$(filter darwin_% linux_% windows_%,$(TARGET))),1,)
EVERY_MIN_DECODERS := 300

# In a trimmed set, decoders, demuxers, parsers, bitstream filters, hwaccels and
# input devices all start empty.
# Left enabled, filters and hwaccels select decoders and parsers back in:
# videotoolbox's hevc, av1 and vp9 hwaccels brought their decoders, and the
# eac3_core and dts2pts filters the ac3 and hevc parsers. A component a set
# keeps still selects what it needs.
ifeq ($(EVERY_CODEC),1)
# Everything FFmpeg has, as before v0.6.0, but one input device.
SET_CONF = --disable-indevs
else
SET_CONF = --disable-decoders --disable-demuxers --disable-parsers --disable-bsfs --disable-hwaccels --disable-indevs \
	--enable-demuxer=$(subst $(space),$(comma),$(strip $(SET_DEMUXERS))) \
	--enable-decoder=$(subst $(space),$(comma),$(strip $(SET_DECODERS))) \
	--enable-parser=$(subst $(space),$(comma),$(strip $(SET_PARSERS)))
ifneq ($(strip $(SET_HWACCELS)),)
	SET_CONF += --enable-hwaccel=$(subst $(space),$(comma),$(strip $(SET_HWACCELS)))
endif
endif
ifneq ($(strip $(SET_INDEVS)),)
	SET_CONF += --enable-indev=$(subst $(space),$(comma),$(strip $(SET_INDEVS)))
endif
ifeq ($(SET),audio)
	SET_CONF += --disable-swscale --disable-avdevice --disable-videotoolbox
endif

FFMPEG_CONF = $(BASE_CONF) $(TARGET_CONF) $(SET_CONF)

# --- Shared libraries -------------------------------------------------------
# Player's ADR-0037 D3 ships FFmpeg as shared libraries on every platform, so a
# user can replace them (LGPL 2.1 §6b; goffmpeg-libs#14). They are the only
# FFmpeg build: goffmpeg links nothing else since #40, and goffmpeg-libs
# v0.4.0 was the last release to carry static archives (#45).
#
# The suffix keeps the names apart from any other FFmpeg in the same process or
# package: a distro libavcodec.so.62 on Linux, an avcodec-62.dll another
# component loaded on Windows, another SDK's libavcodec.so in an APK. FFmpeg's
# checklist (ffmpeg.org/legal.html) allows it: "Do not rename FFmpeg dlls to
# some obfuscated name, but adding a suffix or prefix is fine".
#
# The libraries carry no rpath of their own. Whatever loads them supplies it:
# the executable's rpath on macOS and Linux, the app's lib/<abi> on Android,
# the executable's directory on Windows.
#
# SHARED_SUFFIX and SHARED_DIR_NAME come from the codec set above.
SHARED_BUILD_DIR := $(BUILD_ROOT)/$(TARGET)-$(SHARED_DIR_NAME)
SHARED_STAGE     := $(BUILD_ROOT)/$(TARGET)-$(SHARED_DIR_NAME)-install
SHARED_DIR       := $(DIST_DIR)/$(SHARED_DIR_NAME)

SHARED_CONF = --enable-shared --disable-static --build-suffix=$(SHARED_SUFFIX)
# Apple: @rpath install names, so a consumer finds the libraries through its
# own rpath. The iOS ones are then rewrapped as frameworks.
ifneq ($(filter darwin_% ios_% iossimulator_%,$(TARGET)),)
	SHARED_CONF += --install-name-dir=@rpath
endif
# Android 15 devices can use 16KB pages, and Google Play requires native
# libraries aligned for them. NDK r28+ does this by default; saying it keeps
# an older NDK from quietly producing 4KB-aligned libraries.
ifneq ($(filter android_%,$(TARGET)),)
	SHARED_CONF += --extra-ldsoflags=-Wl,-z,max-page-size=16384
endif
# Same reason as the bridge's -static-libgcc below (#26): without it every DLL
# imports libgcc_s_seh-1.dll, which Windows does not ship.
ifeq ($(TARGET),windows_amd64)
	SHARED_CONF += --extra-ldsoflags=-static-libgcc
endif
# Linux: each library is named libavcodec-goffmpeg-63.so, with the major
# version before the .so, and its soname is the same. cgo cannot link FFmpeg's
# default name, .so.63: it refuses a path ending in .so.63, and from Go 1.27.4
# and 1.26.11 the -l:<file> form too (#59). These are FFmpeg's own naming
# variables, set the way its Windows build sets them (avcodec-63.dll). So the
# soname follows from the one name, and so does the NEEDED entry each other
# library records for it. With no install links, each library is installed
# once, under that name.
ifneq ($(filter linux_%,$(TARGET)),)
	SHARED_MAKEVARS = 'SLIBNAME_WITH_MAJOR=$$(SLIBPREF)$$(FULLNAME)-$$(LIBMAJOR)$$(SLIBSUF)' \
		'SLIB_INSTALL_NAME=$$(SLIBNAME_WITH_MAJOR)' SLIB_INSTALL_LINKS=
endif

.PHONY: all dist dist-all clean clean-all check-src patch-src print-config \
        bridge-shared linux-docker test test-cgo test-purego sync-libs sync-headers check-paths \
        check-shared check-components smoke-shared

# `all` builds the host target only. Use dist-all for the full matrix.
all: dist

check-src:
	@test -n "$(FFMPEG_SRC)" && test -x "$(FFMPEG_SRC)/configure" || { \
		echo "FFmpeg source not found. Clone it next to this repo or set FFMPEG_SRC=/path/to/ffmpeg"; \
		exit 1; }

# Local fixes carried against the pinned FFmpeg revision. Each one is a real
# upstream bug with a real upstream patch pending; this directory is what stops
# us waiting for a release to ship a fix we have already made. Keep it empty
# when you can — every entry is a merge conflict at the next FFmpeg bump, and
# the header of each file says what it fixes and where it went upstream.
PATCH_DIR := $(ROOT)/patches

# Idempotent: dist-all configures seven targets from one source tree, so this
# runs repeatedly against an already-patched checkout. A patch that reverses
# cleanly is one that is already applied, which is the check used here — it
# distinguishes "done" from "does not fit this revision", and only the latter
# is an error.
patch-src: check-src
	@set -e; \
	ls $(PATCH_DIR)/*.patch >/dev/null 2>&1 || exit 0; \
	command -v patch >/dev/null 2>&1 || { \
		echo "ERROR: patch(1) not found, and $(PATCH_DIR) is not empty."; \
		echo "       Install it (Debian: apt-get install patch) and rebuild."; \
		exit 1; }; \
	for p in $(PATCH_DIR)/*.patch; do \
		test -e "$$p" || continue; \
		name=$$(basename "$$p"); \
		if patch -p1 -R -f -s --dry-run -d "$(FFMPEG_SRC)" < "$$p" >/dev/null 2>&1; then \
			echo "    already applied: $$name"; \
		elif patch -p1 -f -s -d "$(FFMPEG_SRC)" < "$$p"; then \
			echo "==> applied: $$name"; \
		else \
			echo "ERROR: $$name does not apply to $(FFMPEG_SRC)"; \
			echo "       FFmpeg has moved under it — rebase the patch or drop it if fixed upstream."; \
			exit 1; \
		fi; \
	done

print-config: check-src
	@echo "TARGET      = $(TARGET)"
	@echo "DIST_DIR    = $(DIST_DIR)"
	@echo "FFMPEG_CONF = $(FFMPEG_CONF) $(SHARED_CONF)"
	$(info MAKEVARS    = $(SHARED_MAKEVARS))

# dist builds the shared libraries for TARGET into $(DIST_DIR)/shared, each
# file named as the loader will look for it (its soname, install name or DLL
# name), with the import libraries beside the DLLs on Windows and one framework
# per library on iOS. FFmpeg's own install tree is staged separately because it
# is built of symlinks, which a Go module zip leaves out; sync-headers reads the
# headers from it.
dist: check-src patch-src
	@mkdir -p "$(SHARED_BUILD_DIR)"
	cd "$(SHARED_BUILD_DIR)" && "$(FFMPEG_SRC)/configure" $(FFMPEG_CONF) $(SHARED_CONF)
	@$(MAKE) --no-print-directory check-components TARGET=$(TARGET) SET=$(SET)
	$(MAKE) -C "$(SHARED_BUILD_DIR)" -j$(NPROC) $(SHARED_MAKEVARS)
	rm -rf "$(SHARED_STAGE)"
	$(MAKE) -C "$(SHARED_BUILD_DIR)" install DESTDIR="$(SHARED_STAGE)" $(SHARED_MAKEVARS)
	IOS_MIN="$(IOS_MIN)" SHARED_SUFFIX="$(SHARED_SUFFIX)" \
		scripts/stage-shared.sh "$(TARGET)" "$(SHARED_STAGE)/$(TARGET)" "$(SHARED_DIR)"
	@$(MAKE) --no-print-directory check-shared TARGET=$(TARGET) SET=$(SET)
	@echo "==> $(TARGET) $(SET) set's shared libraries installed to $(SHARED_DIR)"

# check-components holds the configured build to its codec set's lists (#41):
# configure enables what an enabled component selects, and this fails a build
# that carries a decoder, demuxer, parser, hwaccel or input device the set
# doesn't list. dist runs it after configure, before compiling anything.
check-components:
	DECODERS="$(SET_DECODERS)" DEMUXERS="$(SET_DEMUXERS)" PARSERS="$(SET_PARSERS)" \
		HWACCELS="$(SET_HWACCELS)" INDEVS="$(SET_INDEVS)" \
		$(if $(EVERY_CODEC),AT_LEAST="DECODER DEMUXER PARSER HWACCEL" MIN_DECODERS=$(EVERY_MIN_DECODERS)) \
		scripts/check-components.sh "$(SHARED_BUILD_DIR)"

# check-shared refuses a shared build whose names or links would not load: a
# soname or install name that is not the file's own name, a dependency on a
# library that is neither beside it nor part of the OS, a text relocation
# (Android refuses those), 4KB alignment on Android, a DLL Windows does not
# ship. It is the test that fails when a toolchain or FFmpeg bump changes how
# the libraries are named.
check-shared:
	@test -d "$(SHARED_DIR)" || { echo "no shared build for $(TARGET) — run: make dist TARGET=$(TARGET)"; exit 1; }
	READELF="$(SHARED_READELF)" OBJDUMP="$(SHARED_OBJDUMP)" IOS_MIN="$(IOS_MIN)" GLIBC_FLOOR="$(GLIBC_FLOOR)" SET="$(SET)" \
		scripts/check-shared.sh "$(TARGET)" "$(SHARED_DIR)"

# Tools check-shared reads the binaries with, by target rather than by host.
SHARED_READELF ?= readelf
ifneq ($(filter android_%,$(TARGET)),)
	SHARED_READELF = $(NDK_BIN)/llvm-readelf
endif
SHARED_OBJDUMP ?= objdump
ifeq ($(TARGET),windows_amd64)
	SHARED_OBJDUMP = x86_64-w64-mingw32-objdump
endif
ifeq ($(TARGET),windows_arm64)
	SHARED_OBJDUMP = $(LLVM_MINGW)/bin/llvm-objdump
endif

# smoke-shared links a small C program against the shared libraries alone and,
# where this machine can execute the target, runs it: it decodes a test file
# and checks that the libraries it loaded are the shared build. Linking proves
# the import names resolve; running proves the loader finds each library and
# its dependencies the way a consumer will, through an rpath.
SMOKE_DIR := $(BUILD_ROOT)/$(TARGET)-$(SHARED_DIR_NAME)-smoke
SMOKE_RUN ?= auto
# The media set decodes the video of an MP4, the audio set an MP3.
SMOKE_SAMPLE = $(ROOT)/testdata/$(if $(filter audio,$(SET)),sample_cover.mp3,sample.mp4)
ifeq ($(TARGET),darwin_arm64)
	SMOKE_CC = clang -arch arm64 -mmacosx-version-min=$(MACOS_MIN)
endif
ifeq ($(TARGET),darwin_amd64)
	SMOKE_CC = clang -arch x86_64 -mmacosx-version-min=$(MACOS_MIN)
endif
ifeq ($(TARGET),ios_arm64)
	SMOKE_CC = clang -arch arm64 -isysroot $(IOS_SDK) -mios-version-min=$(IOS_MIN)
endif
ifeq ($(TARGET),iossimulator_arm64)
	SMOKE_CC = clang -arch arm64 -isysroot $(SIM_SDK) -mios-simulator-version-min=$(IOS_MIN)
endif
ifeq ($(TARGET),android_arm64)
	SMOKE_CC = $(NDK_BIN)/aarch64-linux-android$(ANDROID_API)-clang
endif
ifeq ($(TARGET),android_arm)
	SMOKE_CC = $(NDK_BIN)/armv7a-linux-androideabi$(ANDROID_API)-clang
endif
ifeq ($(TARGET),android_amd64)
	SMOKE_CC = $(NDK_BIN)/x86_64-linux-android$(ANDROID_API)-clang
endif
ifneq ($(filter linux_%,$(TARGET)),)
	SMOKE_CC = cc
endif
ifeq ($(TARGET),windows_amd64)
	SMOKE_CC = x86_64-w64-mingw32-gcc
endif
ifeq ($(TARGET),windows_arm64)
	SMOKE_CC = $(LLVM_MINGW)/bin/aarch64-w64-mingw32-gcc
endif

smoke-shared:
	@test -d "$(SHARED_DIR)" || { echo "no shared build for $(TARGET) — run: make dist TARGET=$(TARGET)"; exit 1; }
	SMOKE_CC="$(SMOKE_CC)" SMOKE_RUN="$(SMOKE_RUN)" SMOKE_SET="$(SET)" INCLUDE="$(ROOT)/internal/libav/include" \
		scripts/smoke-shared.sh "$(TARGET)" "$(SHARED_DIR)" "$(SMOKE_DIR)" "$(SMOKE_SAMPLE)"

# Every target this host can build without a container or extra toolchain.
HOST_TARGETS = darwin_arm64 darwin_amd64 ios_arm64 iossimulator_arm64 \
               android_arm64 android_arm android_amd64

dist-all:
	@for t in $(HOST_TARGETS); do \
		echo "=== building $$t ==="; \
		$(MAKE) dist TARGET=$$t || exit 1; \
	done

# Linux artifacts are produced in a container, which avoids needing a cross
# toolchain on the host. Requires Docker with the target platform available
# (arm64 hosts emulate amd64 through qemu and vice versa). DOCKER_GOALS picks
# what it builds, e.g. DOCKER_GOALS="dist smoke-shared".
DOCKER_GOALS ?= dist
# The image's glibc is GLIBC_FLOOR: Ubuntu 22.04 ships 2.35. The build runs as
# root, so what it wrote for TARGET is handed back to the caller at the end.
# Best effort: a chown the host's file sharing refuses must not fail a build
# that succeeded.
LINUX_IMAGE := ubuntu:22.04
linux-docker: check-src
	docker run --rm --platform linux/$(subst linux_,,$(TARGET)) \
		-v "$(ROOT)":/work -v "$(FFMPEG_SRC)":/ffmpeg -w /work \
		$(LINUX_IMAGE) bash -c '\
			set -e; \
			sh scripts/apt-install.sh build-essential yasm nasm pkg-config libssl-dev make patch \
				-- make cc nasm pkg-config patch; \
			trap "chown -R $(shell id -u):$(shell id -g) /work/internal/libav/dist/$(TARGET) /work/internal/libav/build/$(TARGET)-* 2>/dev/null || true" EXIT; \
			ldd --version | head -n 1; \
			make $(DOCKER_GOALS) TARGET=$(TARGET) FFMPEG_SRC=/ffmpeg NPROC=$$(nproc)'

# Toolchain and system libraries for the shared bridge, selected by TARGET
# rather than by host: the Windows bridge is cross-built from Linux.
BRIDGE_CC ?= $(CC)
ifeq ($(TARGET),darwin_arm64)
	BRIDGE_CC = clang -arch arm64
endif
ifeq ($(TARGET),darwin_amd64)
	BRIDGE_CC = clang -arch x86_64
endif
# The bridge loads FFmpeg as shared libraries, like the cgo build (player
# ADR-0037 D3, #40), so these are only what bridge.c itself needs. It finds
# FFmpeg beside itself, in a package, or in ../shared, in this checkout's
# dist/<target> and in a goffmpeg-libs module. Windows has no rpath: the DLLs
# must be beside the executable or on PATH.
BRIDGE_FFMPEG = $(addprefix $(SHARED_DIR)/,$(shell scripts/shared-libs.sh $(TARGET) "$(SHARED_DIR)" | awk '{print $$2}'))
ifneq ($(filter darwin_%,$(TARGET)),)
	BRIDGE_SYSLIBS = -framework AVFoundation -framework CoreMedia -framework Foundation
	# The AVFoundation capture-mode query is Objective-C and so cannot live in
	# bridge.c. cgo picks it up from the package directory on its own; this
	# shared-library link is the path that has to be told, and omitting it
	# fails only at link time with an undefined goffmpeg_avf_collect_modes.
	BRIDGE_EXTRA_SRC = bridge_avf_darwin.m
	# The linker records the output path as the dylib's install name, which
	# put the CI runner's checkout path into the published library. purego
	# opens it by file path, so the install name is never used to find it.
	BRIDGE_LDFLAGS = -install_name @rpath/libgoffmpeg.dylib \
		-Wl,-rpath,@loader_path -Wl,-rpath,@loader_path/../shared
endif
ifneq ($(filter linux_%,$(TARGET)),)
	BRIDGE_LDFLAGS = -Wl,-rpath,'$$ORIGIN:$$ORIGIN/../shared'
	BRIDGE_SYSLIBS = -lpthread -lm
endif
ifeq ($(TARGET),windows_amd64)
	BRIDGE_CC = x86_64-w64-mingw32-gcc
	# mingw-w64 GCC links its runtime as libgcc_s_seh-1.dll by default, which
	# Windows does not ship and the companion module does not carry, so the
	# DLL would load only where MinGW happens to be on PATH (#26). llvm-mingw,
	# used for windows_arm64, has no such dependency.
	BRIDGE_LDFLAGS = -static-libgcc
	BRIDGE_SYSLIBS = $(WIN_SYSLIBS)
endif
ifeq ($(TARGET),windows_arm64)
	BRIDGE_CC = $(LLVM_MINGW)/bin/aarch64-w64-mingw32-gcc
	BRIDGE_SYSLIBS = $(WIN_SYSLIBS)
endif

# dshow capture and Schannel drag in a long tail of import libraries. The set
# was established by linking on real Windows: omitting any one of them fails
# only at link time, which `go build` never reaches for a library package.
WIN_SYSLIBS = -lsecur32 -lbcrypt -lws2_32 -lole32 -loleaut32 -lstrmiids -luuid \
	-lgdi32 -lavicap32 -lvfw32 -luser32 -lshlwapi -lcrypt32 -lncrypt -lmfplat -lmfuuid

# Companion modules under github.com/belluikit/goffmpeg-libs distribute the
# libraries, one module per platform, so a consumer downloads only their own
# target rather than every one. sync-libs copies a freshly built target into a
# sibling checkout of that repository, and writes the module's cgo link files
# from the shared libraries it copied (scripts/gen-libav.sh): the versions in
# their names come from the files, so an FFmpeg bump needs no hand edit.
LIBS_REPO ?= $(ROOT)/../goffmpeg-libs

# The module's lib/ holds only the purego bridge, on desktop. sync-libs removes
# the static archives it carried up to goffmpeg-libs v0.4.0 (#45), and a mobile
# module, which has no bridge, is left with no lib/.
sync-libs: check-paths
	rm -f "$(LIBS_REPO)/$(TARGET)"/lib/*.a
	@if ls "$(DIST_DIR)"/lib/libgoffmpeg.* >/dev/null 2>&1; then \
		mkdir -p "$(LIBS_REPO)/$(TARGET)/lib" && cp "$(DIST_DIR)"/lib/libgoffmpeg.* "$(LIBS_REPO)/$(TARGET)/lib/" && \
		echo "    (including the purego shared bridge)"; fi
	@rmdir "$(LIBS_REPO)/$(TARGET)/lib" 2>/dev/null || true
	for d in shared shared-audio; do \
		rm -rf "$(LIBS_REPO)/$(TARGET)/$$d" && cp -R "$(DIST_DIR)/$$d" "$(LIBS_REPO)/$(TARGET)/$$d" || exit 1; \
	done
	scripts/gen-libav.sh $(TARGET) "$(LIBS_REPO)/$(TARGET)"
	@echo "==> $(TARGET) libraries synced to $(LIBS_REPO)/$(TARGET)"

# check-paths refuses a build whose libraries name a personal home directory.
# Configure lines, toolchain paths and install names all end up inside the
# published binaries, and self-hosted and local builds put their user's name
# there (goffmpeg-libs#6). GitHub-hosted runners build as the generic `runner`,
# which is allowed: the Android NDK and llvm-mingw's own CRT carry such paths
# and cannot be told not to. "FFmpeg version" must be found in the shared
# libavutil, so a scan that reads nothing fails instead of passing.
#
# It reads both codec sets, shared/ and shared-audio/, which a goffmpeg-libs
# module carries together (#41), so both must have been built.
check-paths:
	@for d in shared shared-audio; do \
		test -d "$(DIST_DIR)/$$d" || { echo "no $$d/ for $(TARGET) — run: make dist TARGET=$(TARGET) and make dist TARGET=$(TARGET) SET=audio"; exit 1; }; \
		n=$$(find "$(DIST_DIR)/$$d" -type f -name '*avutil*' -exec grep -la 'FFmpeg version' {} + | wc -l); \
		[ "$$n" -ge 1 ] || { echo "check-paths: 'FFmpeg version' not found in $$d/'s libavutil, so the scan is not reading it"; exit 1; }; \
	done
	@bad=$$(find "$(DIST_DIR)/lib" "$(DIST_DIR)/shared" "$(DIST_DIR)/shared-audio" -type f 2>/dev/null | while read -r f; do \
		LC_ALL=C grep -aoE '/(Users|home)/[A-Za-z0-9._-]+/' "$$f" | grep -vxE '/(Users|home)/runner/' | sed "s|^|$${f#$(DIST_DIR)/}: |"; \
		done | sort | uniq -c); \
	if [ -n "$$bad" ]; then echo "check-paths: $(TARGET) libraries name a personal home directory:"; echo "$$bad"; exit 1; fi
	@echo "==> $(TARGET): no personal home directories in lib/, shared/ or shared-audio/"

# The FFmpeg public headers are identical for every target, so one copy lives
# in this module and all platforms compile against it. They come from the
# shared build's install tree.
sync-headers:
	@test -d "$(SHARED_STAGE)/$(TARGET)/include" || { echo "no build for $(TARGET) — run: make dist TARGET=$(TARGET)"; exit 1; }
	rm -rf "$(ROOT)/internal/libav/include"
	cp -R "$(SHARED_STAGE)/$(TARGET)/include" "$(ROOT)/internal/libav/include"
	@echo "==> headers refreshed from $(TARGET)"

# The purego backend dlopens this. It loads FFmpeg as shared libraries, the
# ones dist builds, so a package carries libgoffmpeg beside them (#40).
#
# Deliberately not dependent on `dist`: the libraries are usually already
# built (or synced from a companion module), and re-running configure+make to
# produce a 75KB wrapper wastes minutes.
bridge-shared:
	@[ "$(SET)" = media ] || { echo "bridge-shared: the purego bridge has video; it links the media set (SET=media)"; exit 1; }
	@test -d "$(SHARED_DIR)" || { \
		echo "no shared libraries for $(TARGET) — run: make dist TARGET=$(TARGET)"; exit 1; }
	@mkdir -p "$(DIST_DIR)/lib"
	$(BRIDGE_CC) -shared -fPIC $(BRIDGE_LDFLAGS) \
		-o "$(DIST_DIR)/lib/libgoffmpeg.$(LIB_EXT)" \
		bridge.c $(BRIDGE_EXTRA_SRC) \
		-I"$(ROOT)/internal/libav/include" \
		$(BRIDGE_FFMPEG) $(BRIDGE_SYSLIBS)
	@if [ -n "$(filter linux_%,$(TARGET))" ]; then \
		scripts/check-glibc.sh $(GLIBC_FLOOR) "$(DIST_DIR)/lib/libgoffmpeg.so"; fi
	@echo "==> $(TARGET) shared bridge at $(DIST_DIR)/lib/libgoffmpeg.$(LIB_EXT)"

test: test-cgo

test-cgo:
	CGO_ENABLED=1 go test ./...

test-purego: bridge-shared
	CGO_ENABLED=0 go test ./...

clean:
	rm -rf "$(DIST_DIR)" "$(SHARED_BUILD_DIR)" "$(SHARED_STAGE)" "$(SMOKE_DIR)"

clean-all:
	rm -rf "$(BUILD_ROOT)" "$(DIST_ROOT)"
