//go:build cgo

package goffmpeg

import (
	"strings"
	"testing"
)

// TestOpenReaderWithOptionsApplied mirrors TestOpenWithOptionsApplied for the
// custom-IO entry point, and for the same reason: only an option libavformat
// rejects can distinguish "applied" from "quietly discarded".
func TestOpenReaderWithOptionsApplied(t *testing.T) {
	dec, err := OpenReaderWithOptions(openSample(t), "probesize=200000,analyzeduration=2000000")
	if err != nil {
		t.Fatalf("OpenReaderWithOptions(valid options): %v", err)
	}
	dec.Close()

	bad, err := OpenReaderWithOptions(openSample(t), "probesize=notanumber")
	if err == nil {
		bad.Close()
		t.Fatal("OpenReaderWithOptions(probesize=notanumber) succeeded — the options were never applied")
	}
}

// TestOpenReaderWithOptionsMalformed checks the reader path reports a malformed
// string the same way the url path does, rather than as a generic open failure.
func TestOpenReaderWithOptionsMalformed(t *testing.T) {
	dec, err := OpenReaderWithOptions(openSample(t), "probesize")
	if err == nil {
		dec.Close()
		t.Fatal("OpenReaderWithOptions(malformed) succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "malformed open options") {
		t.Fatalf("OpenReaderWithOptions(malformed) = %v, want a malformed-options error", err)
	}
}

// TestOpenReaderWithOptionsReleasesReader guards the failure path: a rejected
// open must still drop the reader from the registry. The malformed case returns
// before libavformat is ever called, so it is the one most likely to leak.
func TestOpenReaderWithOptionsReleasesReader(t *testing.T) {
	before := registrySize()

	if dec, err := OpenReaderWithOptions(openSample(t), "probesize"); err == nil {
		dec.Close()
		t.Fatal("OpenReaderWithOptions(malformed) succeeded, want an error")
	}
	if dec, err := OpenReaderWithOptions(openSample(t), "probesize=notanumber"); err == nil {
		dec.Close()
		t.Fatal("OpenReaderWithOptions(bad value) succeeded, want an error")
	}

	if after := registrySize(); after != before {
		t.Fatalf("reader registry holds %d after two failed opens, was %d", after, before)
	}
}
