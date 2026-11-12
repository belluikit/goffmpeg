#!/bin/sh
# Regenerates the GetYCbCr test clips (#64). Each is a few frames of FFmpeg's
# testsrc2 encoded with libx264.
set -e
cd "$(dirname "$0")"
enc() { out=$1; size=$2; shift 2; ffmpeg -loglevel error -y -f lavfi -i "testsrc2=size=$size:rate=10:duration=0.3" -c:v libx264 "$@" "$out"; }
enc bt709_tv.mp4 64x64 -pix_fmt yuv420p -colorspace bt709 -color_primaries bt709 -color_trc bt709 -color_range tv
enc smpte170m_pc.mp4 64x64 -pix_fmt yuv420p -colorspace smpte170m -color_range pc
enc untagged_720.mp4 32x720 -pix_fmt yuv420p
# The same two tags at a size MediaCodec configures, which 64x64 is not, so
# the hardware path's colour tags are checked too (#65).
enc bt709_tv_320.mp4 320x240 -pix_fmt yuv420p -colorspace bt709 -color_primaries bt709 -color_trc bt709 -color_range tv
enc smpte170m_pc_320.mp4 320x240 -pix_fmt yuv420p -colorspace smpte170m -color_range pc
enc yuv444.mp4 64x64 -pix_fmt yuv444p
# A stream that changes resolution partway through: two MPEG-TS segments
# concatenated byte for byte, 64x64 then 96x96.
ffmpeg -loglevel error -y -f lavfi -i testsrc2=size=64x64:rate=10:duration=0.5 -c:v libx264 -pix_fmt yuv420p -f mpegts a.ts
ffmpeg -loglevel error -y -f lavfi -i testsrc2=size=96x96:rate=10:duration=0.5 -c:v libx264 -pix_fmt yuv420p -f mpegts -output_ts_offset 0.5 b.ts
cat a.ts b.ts > size_change.ts
rm a.ts b.ts
