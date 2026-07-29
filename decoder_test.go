package goffmpeg

import (
	"testing"
)

func TestOpen(t *testing.T) {
	// Try to open a non-existent file.
	// This verifies that the library is loaded and the bridge function is callable.
	_, err := Open("non-existent-file.mp4")
	if err == nil {
		t.Error("expected error opening non-existent file, got nil")
	}
}

func TestOpenDeviceUnknownFormat(t *testing.T) {
	// An unknown input-device format resolves to no AVInputFormat, so the
	// bridge returns before touching any hardware. This proves the device
	// path (avdevice_register_all + av_find_input_format) is wired and
	// callable without needing a real camera/permission prompt.
	_, err := OpenDevice("definitely-not-a-real-device", "0", "")
	if err == nil {
		t.Error("expected error opening an unknown device format, got nil")
	}
}
