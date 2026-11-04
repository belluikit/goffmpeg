//go:build cgo

// cgo only: the purego leg dlopens the libgoffmpeg published in
// goffmpeg-libs, which has GOFFMPEG_TRUNCATED only once those libraries are
// rebuilt from this bridge. Until then purego reports such an end as clean,
// as before.

package goffmpeg

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// decodeToEnd reads packets until the end or an error.
func decodeToEnd(t *testing.T, d IDecoder) error {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		mt, err := d.Next()
		if errors.Is(err, ErrAgain) {
			continue
		}
		if err != nil {
			return err
		}
		if mt == MediaTypeNone {
			return nil
		}
	}
	t.Fatal("decoding neither ended nor failed within 30s")
	return nil
}

// TestRefusedReconnectIsTruncatedNotTheEnd (belluikit/player#237): over
// FFmpeg's http, a connection that drops mid-file is reconnected with Range;
// the server refuses that with 403, as it does once a signed link expires.
// FFmpeg itself calls that the end of file, and a player took the song as
// finished. Next must report ErrStreamTruncated. The control serves the file
// whole and must still end cleanly.
func TestRefusedReconnectIsTruncatedNotTheEnd(t *testing.T) {
	data, err := os.ReadFile("testdata/sample_cover.mp3")
	if err != nil {
		t.Fatal(err)
	}
	const opts = "reconnect=1,reconnect_delay_max=2,rw_timeout=5000000"
	var refuse atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !refuse.Load() {
			http.ServeContent(w, r, "s.mp3", time.Time{}, bytes.NewReader(data))
			return
		}
		if r.Header.Get("Range") != "" && r.Header.Get("Range") != "bytes=0-" {
			http.Error(w, "Request has expired", http.StatusForbidden)
			return
		}
		// Half the file, then the connection drops.
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Header().Set("Accept-Ranges", "bytes")
		w.Write(data[:len(data)/2])
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}))
	defer srv.Close()

	t.Run("control: whole file", func(t *testing.T) {
		d, err := OpenWithOptions(srv.URL+"/s.mp3", opts)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		if err := decodeToEnd(t, d); err != nil {
			t.Fatalf("a file served whole ended with %v; want a clean end", err)
		}
	})
	t.Run("dropped, reconnect refused", func(t *testing.T) {
		refuse.Store(true)
		d, err := OpenWithOptions(srv.URL+"/s.mp3", opts)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		if err := decodeToEnd(t, d); !errors.Is(err, ErrStreamTruncated) {
			t.Fatalf("Next = %v at the drop; want ErrStreamTruncated", err)
		}
	})
}
