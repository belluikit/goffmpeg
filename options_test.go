package goffmpeg

import (
	"errors"
	"strings"
	"testing"
)

// Named locally rather than reused: the sampleMedia const lives in the
// cgo-tagged reader tests, and these run under both backends.
const optionsMedia = "testdata/sample.mp4"

// skipIfNoOpenOptions skips when the loaded backend cannot take open options at
// all. The purego leg of CI deliberately stages the *published* libgoffmpeg, so
// until a release carrying decoder_open_opts ships, that library reports
// ErrOpenOptionsUnavailable — a real answer about the artifact, but not one
// these tests can assert libavformat behaviour through.
func skipIfNoOpenOptions(t *testing.T, err error) {
	t.Helper()
	if errors.Is(err, ErrOpenOptionsUnavailable) {
		t.Skipf("backend takes no open options: %v", err)
	}
}

// TestOpenWithOptionsApplied is the test that matters: it proves the options
// string reaches libavformat rather than being parsed and dropped.
//
// A wrong *value* for an option libavformat knows is the only assertion that
// distinguishes the two. A well-formed option that is merely ignored looks
// identical to success, which is exactly the bug this replaces — so the failing
// half is the evidence, and the succeeding half rules out the file simply being
// unopenable.
func TestOpenWithOptionsApplied(t *testing.T) {
	good, err := OpenWithOptions(optionsMedia, "probesize=100000,analyzeduration=1000000")
	skipIfNoOpenOptions(t, err)
	if err != nil {
		t.Fatalf("OpenWithOptions(valid options): %v", err)
	}
	good.Close()

	// probesize is an int64 option; "notanumber" fails av_opt_set, which
	// aborts avformat_open_input. Reaching this error means the dictionary was
	// applied to the format context.
	bad, err := OpenWithOptions(optionsMedia, "probesize=notanumber")
	if err == nil {
		bad.Close()
		t.Fatal("OpenWithOptions(probesize=notanumber) succeeded — the options were never applied")
	}
}

// TestOpenWithOptionsIgnoresInapplicable pins the behaviour a caller depends on
// when it holds one options string for whatever the user typed: options meant
// for a protocol that is not in play must be ignored, not rejected. Handing a
// local path the reconnect/timeout set intended for http has to keep working.
func TestOpenWithOptionsIgnoresInapplicable(t *testing.T) {
	const netOpts = "rw_timeout=5000000,reconnect=1,reconnect_streamed=1," +
		"reconnect_delay_max=5,user_agent=goffmpeg-test,http_persistent=1"

	dec, err := OpenWithOptions(optionsMedia, netOpts)
	skipIfNoOpenOptions(t, err)
	if err != nil {
		t.Fatalf("OpenWithOptions(%q) on a local file: %v", netOpts, err)
	}
	defer dec.Close()

	if info := dec.VideoInfo(); info.Width <= 0 || info.Height <= 0 {
		t.Fatalf("decoded %dx%d, want the fixture's real frame size", info.Width, info.Height)
	}
}

// TestOpenWithOptionsMalformed covers the one input the bridge rejects outright:
// a token av_dict_parse_string cannot split into key and value. It is reported
// as its own error rather than an open failure, because the cause is the
// caller's string and not the media.
func TestOpenWithOptionsMalformed(t *testing.T) {
	dec, err := OpenWithOptions(optionsMedia, "rw_timeout")
	if err == nil {
		dec.Close()
		t.Fatal("OpenWithOptions(malformed) succeeded, want an error")
	}
	skipIfNoOpenOptions(t, err)
	if !strings.Contains(err.Error(), "malformed open options") {
		t.Fatalf("OpenWithOptions(malformed) = %v, want a malformed-options error", err)
	}
}

// TestOpenWithOptionsEmpty checks the compatibility path — the one every
// existing caller takes through Open — still opens the file and, on a purego
// backend too old for options, does not report ErrOpenOptionsUnavailable for
// asking for nothing.
func TestOpenWithOptionsEmpty(t *testing.T) {
	dec, err := OpenWithOptions(optionsMedia, "")
	if err != nil {
		t.Fatalf("OpenWithOptions(%q, \"\"): %v", optionsMedia, err)
	}
	defer dec.Close()

	byPath, err := Open(optionsMedia)
	if err != nil {
		t.Fatalf("Open(%q): %v", optionsMedia, err)
	}
	defer byPath.Close()

	if got, want := dec.VideoInfo(), byPath.VideoInfo(); got != want {
		t.Fatalf("OpenWithOptions(\"\") reported %+v, Open reported %+v", got, want)
	}
}
