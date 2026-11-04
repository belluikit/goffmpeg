//go:build cgo

package goffmpeg

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These tests are cgo-only because the purego leg of CI loads the published
// libgoffmpeg, whose bridge predates decoder_interrupt: there Interrupt does
// nothing. The bridge code under test is the same for both backends.

// longMP3 is about three minutes of MP3 built from the one-second fixture: its
// audio frames, without the ID3 tag and without the LAME Info frame, repeated.
// With no Info frame there is no seek table, so a far seek is answered the way
// a network MP3 without one is: from the bitrate, by reading.
func longMP3(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/sample_cover.mp3")
	if err != nil {
		t.Skipf("fixture unavailable: %v", err)
	}
	// The ID3v2 size is four 7-bit bytes, after the 10-byte header.
	tag := 10 + int(data[6])<<21 | int(data[7])<<14 | int(data[8])<<7 | int(data[9])
	frames := data[tag:]
	// The first frame is the Info frame; skip to the next frame sync.
	next := bytes.Index(frames[4:], []byte{0xff, 0xfb})
	if !bytes.Contains(frames[:64], []byte("Info")) || next < 0 {
		t.Fatal("fixture layout changed: no Info frame where expected")
	}
	frames = frames[4+next:]
	return bytes.Repeat(frames, 180)
}

// stallingServer serves media over http, honouring Range, until stall is set;
// from then on a response sends nothing more and waits for the client to go.
type stallingServer struct {
	*httptest.Server
	stall atomic.Bool
	// gone ends every stalled response, so a failing test can still finish.
	gone     chan struct{}
	goneOnce sync.Once
}

func (s *stallingServer) unstick() { s.goneOnce.Do(func() { close(s.gone) }) }

func newStallingServer(t *testing.T, media []byte) *stallingServer {
	s := &stallingServer{gone: make(chan struct{})}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "media.mp3", time.Time{}, &stallReader{
			ReadSeeker: bytes.NewReader(media), stall: &s.stall, ctx: r.Context(), gone: s.gone,
		})
	}))
	t.Cleanup(s.Close)
	return s
}

type stallReader struct {
	io.ReadSeeker
	stall *atomic.Bool
	ctx   context.Context
	gone  chan struct{}
}

func (r *stallReader) Read(p []byte) (int, error) {
	if r.stall.Load() {
		select {
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		case <-r.gone:
			return 0, io.ErrUnexpectedEOF
		}
	}
	// Small reads, so the server cannot have pushed most of the file into the
	// socket before the stall.
	if len(p) > 4096 {
		p = p[:4096]
	}
	return r.ReadSeeker.Read(p)
}

// interruptAfter runs op, trips Interrupt part way, then calls then (if not
// nil), and reports how long op took to return after the interrupt. op must
// still be blocked when Interrupt is called. If op does not return, unstick
// must end whatever blocks it, so the test fails rather than hangs: closing
// the decoder under a call still inside it is not safe.
func interruptAfter(t *testing.T, d IDecoder, op func() error, then, unstick func()) (time.Duration, error) {
	t.Helper()
	type result struct {
		err error
		at  time.Time
	}
	done := make(chan result, 1)
	go func() {
		err := op()
		done <- result{err, time.Now()}
	}()
	time.Sleep(100 * time.Millisecond)
	select {
	case r := <-done:
		t.Fatalf("the operation returned before the interrupt (err %v); the test is not blocking it", r.err)
	default:
	}
	tripped := time.Now()
	d.Interrupt()
	if then != nil {
		then()
	}
	select {
	case r := <-done:
		return r.at.Sub(tripped), r.err
	case <-time.After(5 * time.Second):
		unstick()
		<-done
		t.Fatal("the operation did not return within 5 s of Interrupt")
		return 0, nil
	}
}

// TestInterruptSeekOverHTTP is the case Interrupt exists for: a seek blocked on
// a server that has stopped sending, with no rw_timeout to end it, returns
// promptly once interrupted, and fails rather than claiming to have landed.
func TestInterruptSeekOverHTTP(t *testing.T) {
	srv := newStallingServer(t, longMP3(t))
	d, err := Open(srv.URL + "/media.mp3")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()
	if d.Duration() < 120 {
		t.Fatalf("duration %v s; the test needs a long file", d.Duration())
	}

	srv.stall.Store(true)
	took, err := interruptAfter(t, d, func() error { return d.Seek(d.Duration() * 0.75) }, nil, srv.unstick)
	t.Logf("Seek returned %v after Interrupt: %v", took, err)
	if took > time.Second {
		t.Errorf("Seek returned %v after Interrupt, want promptly", took)
	}
	if err == nil {
		t.Error("an interrupted Seek reported success")
	}
	// Every later read fails too.
	if mt, err := d.Next(); err == nil && mt != MediaTypeNone {
		t.Errorf("Next after Interrupt = %v, nil; want a failure", mt)
	}
}

// TestInterruptNextOverHTTP covers a read blocked between seeks: Next on a
// stalled server.
func TestInterruptNextOverHTTP(t *testing.T) {
	srv := newStallingServer(t, longMP3(t))
	d, err := Open(srv.URL + "/media.mp3")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer d.Close()

	srv.stall.Store(true)
	took, err := interruptAfter(t, d, func() error {
		for {
			mt, err := d.Next()
			if err != nil || mt == MediaTypeNone {
				return err
			}
		}
	}, nil, srv.unstick)
	t.Logf("Next returned %v after Interrupt: %v", took, err)
	if took > time.Second {
		t.Errorf("Next returned %v after Interrupt, want promptly", took)
	}
}

// gateReader is a seekable reader whose Read blocks while gated, until
// released, and counts the reads that start after the release.
type gateReader struct {
	*bytes.Reader
	mu       sync.Mutex
	gated    bool
	released chan struct{}
	after    atomic.Int32
}

func (r *gateReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	gated := r.gated
	r.mu.Unlock()
	if gated {
		select {
		case <-r.released:
			r.after.Add(1)
		default:
			<-r.released
		}
	}
	return r.Reader.Read(p)
}

// TestInterruptReader covers OpenReader. Interrupt cannot end a Read already
// in progress, so the caller ends it, here by releasing the gate; the read
// then delivers data as if nothing happened. Custom IO never consults FFmpeg's
// interrupt callback, so without the reader-side check FFmpeg would carry on
// reading through the file. It must stop instead, without calling r again.
func TestInterruptReader(t *testing.T) {
	r := &gateReader{Reader: bytes.NewReader(longMP3(t)), released: make(chan struct{})}
	d, err := OpenReader(r)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer d.Close()

	r.mu.Lock()
	r.gated = true
	r.mu.Unlock()
	took, err := interruptAfter(t, d, func() error {
		if err := d.Seek(d.Duration() * 0.75); err != nil {
			return err
		}
		for {
			mt, err := d.Next()
			if err != nil || mt == MediaTypeNone {
				return err
			}
		}
	}, func() { close(r.released) }, func() {})
	t.Logf("returned %v after Interrupt: %v; %d reads after the release", took, err, r.after.Load())
	if took > time.Second {
		t.Errorf("returned %v after Interrupt, want promptly", took)
	}
	if err == nil {
		t.Error("an interrupted Seek reported success")
	}
	if n := r.after.Load(); n > 0 {
		t.Errorf("%d reads started after Interrupt; want none", n)
	}
}
