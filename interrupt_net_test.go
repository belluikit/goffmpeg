package goffmpeg

import (
	"os"
	"testing"
	"time"
)

// TestInterruptFarSeekOverNetwork measures what Interrupt is for, on a real
// server: a far seek in an MP3 with no seek table, which FFmpeg answers by
// reading the file through to the target, interrupted part way. It needs the
// network, so it runs only with GOFFMPEG_NET_TESTS=1.
//
// The uninterrupted seek is timed first, so a fast network (where the seek
// ends before the interrupt) shows up as a skip rather than as a pass.
func TestInterruptFarSeekOverNetwork(t *testing.T) {
	if os.Getenv("GOFFMPEG_NET_TESTS") == "" {
		t.Skip("set GOFFMPEG_NET_TESTS=1 to run against the network")
	}
	url := os.Getenv("GOFFMPEG_NET_URL")
	if url == "" {
		url = "https://www.soundhelix.com/examples/mp3/SoundHelix-Song-1.mp3"
	}
	const opts = "reconnect=1,reconnect_delay_max=2,rw_timeout=5000000"
	const after = 100 * time.Millisecond

	open := func() IDecoder {
		d, err := OpenWithOptions(url, opts)
		if err != nil {
			t.Fatalf("open %s: %v", url, err)
		}
		return d
	}

	d := open()
	target := d.Duration() * 0.75
	start := time.Now()
	err := d.Seek(target)
	full := time.Since(start)
	d.Close()
	t.Logf("uninterrupted seek to %.1f s: %v (err %v)", target, full, err)
	if full < 4*after {
		t.Skipf("the seek took only %v; too fast to interrupt part way", full)
	}

	d = open()
	defer d.Close()
	done := make(chan time.Time, 1)
	var seekErr error
	start = time.Now()
	go func() {
		seekErr = d.Seek(target)
		done <- time.Now()
	}()
	time.Sleep(after)
	tripped := time.Now()
	d.Interrupt()
	select {
	case ended := <-done:
		t.Logf("interrupted seek: tripped at %v, returned %v after the interrupt (seek err %v)",
			tripped.Sub(start), ended.Sub(tripped), seekErr)
		if ended.Before(tripped) {
			t.Skip("the seek ended before the interrupt")
		}
		if ended.Sub(tripped) > 500*time.Millisecond {
			t.Errorf("Seek returned %v after Interrupt", ended.Sub(tripped))
		}
		if seekErr == nil {
			t.Error("an interrupted seek reported success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Seek did not return within 10 s of Interrupt")
	}
}
