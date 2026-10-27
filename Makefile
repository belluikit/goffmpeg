FFMPEG_SRC ?= $(shell cd ../ffmpeg 2>/dev/null && pwd)
ROOT       := $(shell pwd)
DIST_ROOT  := $(ROOT)/internal/libav/dist
BUILD_ROOT := $(ROOT)/internal/libav/build
NPROC      := $(shell sysctl -n hw.ncpu 2>/dev/null || nproc)

# ---------------------------------------------------------------------------
# Target matrix
#
# Artifacts are laid out per platform as dist/<GOOS>_<GOARCH>/{include,lib}, so
# one checkout can hold every target at once and cgo picks the right one via
# the per-platform files in libav_*.go.
#
# Build one with:  make dist TARGET=darwin_amd64
# Build the host:  make dist
# ---------------------------------------------------------------------------

HOST_ARCH := $(shell uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
HOST_OS   := $(shell uname -s | tr 'A-Z' 'a-z' | sed 's/^darwin$$/darwin/;s/^linux$$/linux/')
TARGET    ?= $(HOST_OS)_$(HOST_ARCH)

DIST_DIR  := $(DIST_ROOT)/$(TARGET)
BUILD_DIR := $(BUILD_ROOT)/$(TARGET)

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
APPLE_CONF = --enable-securetransport --enable-videotoolbox \
	--enable-hwaccel=h264_videotoolbox,hevc_videotoolbox,vp9_videotoolbox,av1_videotoolbox

MACOS_CONF = $(APPLE_CONF) --enable-avdevice --enable-indev=avfoundation --enable-avfoundation

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
ifeq ($(TARGET),linux_amd64)
	TARGET_CONF = --arch=x86_64 --target-os=linux --enable-openssl --enable-avdevice --enable-indev=v4l2
	LIB_EXT = so
endif

ifeq ($(TARGET),linux_arm64)
	TARGET_CONF = --arch=aarch64 --target-os=linux --enable-openssl --enable-avdevice --enable-indev=v4l2
	LIB_EXT = so
endif

# --- Windows ----------------------------------------------------------------
ifeq ($(TARGET),windows_amd64)
	TARGET_CONF = --arch=x86_64 --target-os=mingw32 --enable-cross-compile \
		--cross-prefix=x86_64-w64-mingw32- --enable-schannel \
		--enable-avdevice --enable-indev=dshow
	LIB_EXT = dll
endif

# Windows on ARM needs llvm-mingw; the GNU mingw-w64 toolchain in the distro
# repositories only targets x86. Set LLVM_MINGW to an unpacked release from
# https://github.com/mstorsjo/llvm-mingw/releases.
LLVM_MINGW ?= /opt/llvm-mingw
ifeq ($(TARGET),windows_arm64)
	TARGET_CONF = --arch=aarch64 --target-os=mingw32 --enable-cross-compile \
		--cross-prefix=$(LLVM_MINGW)/bin/aarch64-w64-mingw32- --enable-schannel \
		--enable-avdevice --enable-indev=dshow
	LIB_EXT = dll
endif

ifndef TARGET_CONF
$(error unknown TARGET "$(TARGET)" — see the target matrix in this Makefile)
endif

FFMPEG_CONF = $(BASE_CONF) $(TARGET_CONF)

.PHONY: all dist dist-all clean clean-all check-src patch-src print-config \
        bridge-shared linux-docker test test-cgo test-purego sync-libs sync-headers check-paths

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
	@echo "FFMPEG_CONF = $(FFMPEG_CONF)"

# dist builds the static libraries for TARGET. Static is what the cgo build
# links against; the shared bridge below exists only for the purego backend.
dist: check-src patch-src
	@mkdir -p "$(BUILD_DIR)"
	cd "$(BUILD_DIR)" && "$(FFMPEG_SRC)/configure" $(FFMPEG_CONF) --enable-static --disable-shared
	$(MAKE) -C "$(BUILD_DIR)" -j$(NPROC)
	$(MAKE) -C "$(BUILD_DIR)" install DESTDIR="$(DIST_ROOT)"
	@echo "==> $(TARGET) installed to $(DIST_DIR)"

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
# (arm64 hosts emulate amd64 through qemu and vice versa).
linux-docker: check-src
	docker run --rm --platform linux/$(subst linux_,,$(TARGET)) \
		-v "$(ROOT)":/work -v "$(FFMPEG_SRC)":/ffmpeg -w /work \
		debian:bookworm-slim bash -c '\
			set -e; \
			apt-get update -qq && apt-get install -y -qq --no-install-recommends \
				build-essential yasm nasm pkg-config libssl-dev make patch >/dev/null; \
			make dist TARGET=$(TARGET) FFMPEG_SRC=/ffmpeg NPROC=$$(nproc)'

# Toolchain and system libraries for the shared bridge, selected by TARGET
# rather than by host: the Windows bridge is cross-built from Linux.
BRIDGE_CC ?= $(CC)
ifeq ($(TARGET),darwin_arm64)
	BRIDGE_CC = clang -arch arm64
endif
ifeq ($(TARGET),darwin_amd64)
	BRIDGE_CC = clang -arch x86_64
endif
ifneq ($(filter darwin_%,$(TARGET)),)
	BRIDGE_SYSLIBS = -framework AVFoundation -framework CoreMedia -framework CoreVideo \
		-framework CoreFoundation -framework Foundation -framework CoreGraphics \
		-framework VideoToolbox -framework Security
	# The AVFoundation capture-mode query is Objective-C and so cannot live in
	# bridge.c. cgo picks it up from the package directory on its own; this
	# shared-library link is the path that has to be told, and omitting it
	# fails only at link time with an undefined goffmpeg_avf_collect_modes.
	BRIDGE_EXTRA_SRC = bridge_avf_darwin.m
	# The linker records the output path as the dylib's install name, which
	# put the CI runner's checkout path into the published library. purego
	# opens it by file path, so the install name is never used to find it.
	BRIDGE_LDFLAGS = -install_name @rpath/libgoffmpeg.dylib
endif
ifneq ($(filter linux_%,$(TARGET)),)
	# -Bsymbolic binds the FFmpeg symbols inside this object rather than
	# leaving them interposable. Without it the link fails on both x86_64 and
	# aarch64: the hand-written assembly addresses global tables with
	# page-relative relocations, which are only valid when the symbol cannot
	# be replaced at load time ("may bind externally ... recompile with -fPIC",
	# which is a misleading message — the objects are already PIC).
	BRIDGE_LDFLAGS = -Wl,-Bsymbolic
	BRIDGE_SYSLIBS = -lssl -lcrypto -lpthread -lm
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
# static libraries, one module per platform, so a consumer downloads only their
# own target rather than every one. sync-libs copies a freshly built target
# into a sibling checkout of that repository.
LIBS_REPO ?= $(ROOT)/../goffmpeg-libs

sync-libs: check-paths
	@test -d "$(DIST_DIR)/lib" || { echo "no build for $(TARGET) — run: make dist TARGET=$(TARGET)"; exit 1; }
	@mkdir -p "$(LIBS_REPO)/$(TARGET)/lib"
	cp "$(DIST_DIR)"/lib/*.a "$(LIBS_REPO)/$(TARGET)/lib/"
	@cp "$(DIST_DIR)"/lib/libgoffmpeg.* "$(LIBS_REPO)/$(TARGET)/lib/" 2>/dev/null && \
		echo "    (including the purego shared bridge)" || true
	@echo "==> $(TARGET) libraries synced to $(LIBS_REPO)/$(TARGET)"

# check-paths refuses a build whose libraries name a personal home directory.
# Configure lines, toolchain paths and install names all end up inside the
# published binaries, and self-hosted and local builds put their user's name
# there (goffmpeg-libs#6). GitHub-hosted runners build as the generic `runner`,
# which is allowed: the Android NDK and llvm-mingw's own CRT carry such paths
# and cannot be told not to. "FFmpeg version" must be found in libavutil.a, so
# a scan that reads nothing fails instead of passing.
check-paths:
	@test -d "$(DIST_DIR)/lib" || { echo "no build for $(TARGET) — run: make dist TARGET=$(TARGET)"; exit 1; }
	@LC_ALL=C grep -qa 'FFmpeg version' "$(DIST_DIR)/lib/libavutil.a" || { \
		echo "check-paths: 'FFmpeg version' not found in libavutil.a, so the scan is not reading the libraries"; exit 1; }
	@bad=$$(for f in "$(DIST_DIR)"/lib/*; do \
		LC_ALL=C grep -aoE '/(Users|home)/[A-Za-z0-9._-]+/' "$$f" | grep -vxE '/(Users|home)/runner/' | sed "s|^|$$(basename $$f): |"; \
		done | sort | uniq -c); \
	if [ -n "$$bad" ]; then echo "check-paths: $(TARGET) libraries name a personal home directory:"; echo "$$bad"; exit 1; fi
	@echo "==> $(TARGET): no personal home directories in lib/"

# The FFmpeg public headers are identical for every target, so one copy lives
# in this module and all platforms compile against it.
sync-headers:
	@test -d "$(DIST_DIR)/include" || { echo "no build for $(TARGET)"; exit 1; }
	rm -rf "$(ROOT)/internal/libav/include"
	cp -R "$(DIST_DIR)/include" "$(ROOT)/internal/libav/include"
	@echo "==> headers refreshed from $(TARGET)"

# The purego backend dlopens this. It embeds the static FFmpeg libraries, so the
# result is one self-contained shared object per target.
#
# Deliberately not dependent on `dist`: the libraries are usually already built
# (or synced from a companion module), and re-running configure+make to produce
# a 35KB wrapper wastes minutes.
bridge-shared:
	@test -d "$(DIST_DIR)/lib" || { \
		echo "no libraries for $(TARGET) — run: make dist TARGET=$(TARGET)"; exit 1; }
	$(BRIDGE_CC) -shared -fPIC $(BRIDGE_LDFLAGS) \
		-o "$(DIST_DIR)/lib/libgoffmpeg.$(LIB_EXT)" \
		bridge.c $(BRIDGE_EXTRA_SRC) \
		-I"$(ROOT)/internal/libav/include" -L"$(DIST_DIR)/lib" \
		-lavdevice -lavformat -lavcodec -lswscale -lswresample -lavutil -lm \
		$(BRIDGE_SYSLIBS)
	@echo "==> $(TARGET) shared bridge at $(DIST_DIR)/lib/libgoffmpeg.$(LIB_EXT)"

test: test-cgo

test-cgo:
	CGO_ENABLED=1 go test ./...

test-purego: bridge-shared
	CGO_ENABLED=0 go test ./...

clean:
	rm -rf "$(BUILD_DIR)" "$(DIST_DIR)"

clean-all:
	rm -rf "$(BUILD_ROOT)" "$(DIST_ROOT)"
