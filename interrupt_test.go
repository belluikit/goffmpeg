package goffmpeg

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

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

// stallingServer serves media over http, honouring Range, but only what Open
// needs: the first openBytes and the last tailBytes. A read of anything in
// between sends nothing and waits for the client to go, so a Seek into the
// middle or a Next past the start blocks on the server.
//
// The stall is by position, not by time (#61). It used to start when the test
// set a flag after Open, and until then the server kept copying the file into
// the connection. Linux's loopback buffers hold the whole 1.5 MB file, so the
// decoder could read to the end, or seek within data already sent, without
// ever blocking, and the tests failed their own precondition: 1 run in 50 on
// native linux/arm64, more on CI and under emulation.
type stallingServer struct {
	*httptest.Server
	// gone ends every stalled response, so a failing test can still finish.
	gone     chan struct{}
	goneOnce sync.Once
}

// openBytes is what Open may read from the start of longMP3: measured, it
// needs between 8 and 16 KB (probing and the first frames), so 128 KB leaves
// room for a build that probes more. tailBytes covers the ID3v1 check, which
// reads the last 128 bytes.
const (
	openBytes = 128 << 10
	tailBytes = 128
)

func (s *stallingServer) unstick() { s.goneOnce.Do(func() { close(s.gone) }) }

func newStallingServer(t *testing.T, media []byte) *stallingServer {
	s := &stallingServer{gone: make(chan struct{})}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "media.mp3", time.Time{}, &stallReader{
			ReadSeeker: bytes.NewReader(media), ctx: r.Context(), gone: s.gone,
			lo: openBytes, hi: int64(len(media)) - tailBytes,
		})
	}))
	t.Cleanup(s.Close)
	return s
}

// stallReader serves its ReadSeeker except for the bytes in [lo, hi), where a
// read waits for the request to end or for gone. http.ServeContent seeks it to
// the start of the requested range, so it tracks the position.
type stallReader struct {
	io.ReadSeeker
	pos, lo, hi int64
	ctx         context.Context
	gone        chan struct{}
}

func (r *stallReader) Seek(offset int64, whence int) (int64, error) {
	pos, err := r.ReadSeeker.Seek(offset, whence)
	r.pos = pos
	return pos, err
}

func (r *stallReader) Read(p []byte) (int, error) {
	if r.pos >= r.lo && r.pos < r.hi {
		select {
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		case <-r.gone:
			return 0, io.ErrUnexpectedEOF
		}
	}
	// Stop at lo, so the stalled bytes are never sent.
	if r.pos < r.lo && int64(len(p)) > r.lo-r.pos {
		p = p[:r.lo-r.pos]
	}
	n, err := r.ReadSeeker.Read(p)
	r.pos += int64(n)
	return n, err
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
