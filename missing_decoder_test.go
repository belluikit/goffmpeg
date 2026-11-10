package goffmpeg

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// requireMissingDecoderSupport skips under CGO_ENABLED=0 when the loaded
// libgoffmpeg predates #41's exports: one from goffmpeg-libs before v0.6.0.
func requireMissingDecoderSupport(t *testing.T) {
	t.Helper()
	if !missingDecoderSupported() {
		t.Skip("the loaded libgoffmpeg exports no decoder_missing or decoder_open_opts_err (#41)")
	}
}

// An input no demuxer in the build recognizes fails to open, and says so
// (#41): a file of zeros and a text file. TestOpenReaderUnrecognizedInput is
// the same through OpenReader.
func TestOpenUnrecognizedInput(t *testing.T) {
	requireMissingDecoderSupport(t)
	dir := t.TempDir()
	inputs := map[string][]byte{
		"zeros.bin": make([]byte, 64<<10),
		"notes.bin": bytes.Repeat([]byte("not media at all\n"), 400),
	}
	for name, data := range inputs {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		dec, err := Open(path)
		if err == nil {
			dec.Close()
			t.Errorf("%s opened", name)
			continue
		}
		if !errors.Is(err, ErrUnrecognizedInput) {
			t.Errorf("%s: %v, want ErrUnrecognizedInput", name, err)
		}
		var av AVError
		if !errors.As(err, &av) || av != avErrorInvalidData {
			t.Errorf("%s: %v carries no AVERROR_INVALIDDATA", name, err)
		}
	}
}

// A URL whose scheme the build has no protocol for reports
// AVERROR_PROTOCOL_NOT_FOUND, by name: what https gives on Android, which is
// built without TLS.
func TestOpenUnknownProtocolReportsIt(t *testing.T) {
	requireMissingDecoderSupport(t)
	dec, err := Open("goffmpegnoproto://example.invalid/a.mp4")
	if err == nil {
		dec.Close()
		t.Fatal("a URL with no protocol opened")
	}
	var av AVError
	if !errors.As(err, &av) || av != avErrorProtocolNotFound {
		t.Fatalf("%v, want AVERROR_PROTOCOL_NOT_FOUND (%d)", err, int(avErrorProtocolNotFound))
	}
	if !strings.Contains(err.Error(), "protocol not found") {
		t.Errorf("%q doesn't say protocol not found", err)
	}
}

// A file that isn't there reports FFmpeg's own error, AVERROR(ENOENT), where
// the open used to say only "failed to open decoder: -1".
func TestOpenMissingFileReportsENOENT(t *testing.T) {
	requireMissingDecoderSupport(t)
	dec, err := Open(filepath.Join(t.TempDir(), "absent.mp4"))
	if err == nil {
		dec.Close()
		t.Fatal("a missing file opened")
	}
	var av AVError
	if !errors.As(err, &av) || av != -2 {
		t.Errorf("%v, want AVError -2 (ENOENT)", err)
	}
	if errors.Is(err, ErrUnrecognizedInput) || errors.Is(err, ErrNoDecoder) {
		t.Errorf("%v is reported as a format problem", err)
	}
}
