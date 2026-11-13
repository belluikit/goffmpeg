//go:build cgo && !goffmpeg_novideo

package goffmpeg

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// stallPatchMark is the log line of patches/0001, which bounds a MediaCodec
// stall before the first frame (#88). Its presence in libavcodec is how the
// test tells a build with the patch from one without (scripts/check-shared.sh
// checks the same string).
const stallPatchMark = "MediaCodec took no input and returned no frame for"

// loadedLibavcodec returns the path of the libavcodec this process mapped,
// from /proc/self/maps, or "" when there is none to find.
func loadedLibavcodec() string {
	f, err := os.Open("/proc/self/maps")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 6 {
			continue
		}
		if p := fields[len(fields)-1]; strings.Contains(p, "libavcodec-goffmpeg") {
			return p
		}
	}
	return ""
}

// A MediaCodec that holds every input buffer and returns no frame ends in a
// fallback to software decoding, not in a decode that never returns (#88).
// The child runs without the binder thread pool the bridge starts (#86),
// which is how a Pixel 9a's MediaCodec stalls: it takes two packets and then
// returns nothing. patches/0001 gives up after 2 s of that, and the bridge
// sends the packets again to the software decoder. A device whose MediaCodec
// needs no pool (moto g power 2024, OnePlus Pad 2) decodes on MediaCodec, and
// the frames are the same either way.
func TestHardwareDecodeStall(t *testing.T) {
	if runtime.GOOS != "android" {
		t.Skip("MediaCodec is Android's")
	}
	lib := loadedLibavcodec()
	if lib == "" {
		t.Fatal("no libavcodec-goffmpeg in /proc/self/maps")
	}
	data, err := os.ReadFile(lib)
	if err != nil {
		t.Fatal(err)
	}
	// goffmpeg-libs v0.9.1 and later carry the patch. Without it, the
	// stalled child below would only end at the 60 s timeout.
	if !bytes.Contains(data, []byte(stallPatchMark)) {
		t.Fatal(lib + " has no patches/0001 (#88): it is older than goffmpeg-libs v0.9.1")
	}
	// run returns the child's frames and hash and the decoder it reported,
	// and how long it took.
	run := func(env ...string) (frames, decoder string, took time.Duration) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHardwareDecodeChild$", "-test.count=1", "-test.v")
		cmd.Env = append(append(os.Environ(), hwChildEnv+"=1"), env...)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		start := time.Now()
		err := cmd.Run()
		took = time.Since(start)
		if ctx.Err() != nil {
			t.Fatalf("child with %v did not finish in 60 s: the stall was not bounded\n%s", env, out.String())
		}
		if err != nil {
			t.Fatalf("child with %v: %v\n%s", env, err, out.String())
		}
		for _, line := range strings.Split(out.String(), "\n") {
			if line, ok := strings.CutPrefix(line, "HWCHILD "); ok {
				if frames, decoder, ok = strings.Cut(line, " decoder="); ok {
					return frames, decoder, took
				}
				t.Fatalf("child with %v printed %q", env, line)
			}
		}
		t.Fatalf("child with %v printed no result:\n%s", env, out.String())
		return "", "", 0
	}
	swFrames, _, _ := run("GOFFMPEG_HWDECODE=0")
	hwFrames, hwDecoder, took := run("GOFFMPEG_HWDECODE=force", "GOFFMPEG_TEST_NO_BINDER_POOL=1")
	t.Logf("no binder pool: %s, decoder %s, in %v", hwFrames, hwDecoder, took.Round(time.Millisecond))
	if hwFrames != swFrames {
		t.Errorf("no binder pool: %s, software %s", hwFrames, swFrames)
	}
	if hwDecoder != `"h264_mediacodec"` && hwDecoder != `"h264 (after h264_mediacodec failed)"` {
		t.Errorf("no binder pool: decoded by %s, want MediaCodec or the fallback from it", hwDecoder)
	}
}
