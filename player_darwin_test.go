//go:build darwin

package avfoundation

import (
	"errors"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-macos/objc"
)

// TestMain runs the MAIN thread's run loop while the tests run on theirs.
//
// This is not scaffolding for its own sake -- it is the smallest possible
// version of what every real consumer of [Player] must do. AVFoundation loads a
// file through the main dispatch queue, which only the main thread's run loop
// drains, and a Go test binary parks its main goroutine inside m.Run while every
// test runs on some other thread. Measured, without this: the live player tests
// pass when run alone and fail the moment ANY earlier test has touched
// AVFoundation, with the item stuck at status 0 forever. The same shape of
// failure, reproduced outside the test binary, is what put the main-thread rule
// in the [Player] documentation.
func TestMain(m *testing.M) {
	runtime.LockOSThread()
	if err := loadPlayer(); err != nil {
		// Nothing to pump; let the tests report the failure themselves.
		os.Exit(m.Run())
	}
	pumpMain.Store(true)
	code := make(chan int, 1)
	go func() { code <- m.Run() }()
	for {
		select {
		case c := <-code:
			os.Exit(c)
		default:
		}
		if !pumpMain.Load() {
			// A test has asked for the main run loop to STOP, so it can show what
			// depends on it. Sleep the same amount instead, so the only thing that
			// changed is the run loop.
			time.Sleep(10 * time.Millisecond)
			continue
		}
		cfRunLoopRunInMode(runLoopMode, 0.01, false)
	}
}

// pumpMain gates TestMain's pumping of the main run loop. Only
// [TestLiveLoadNeedsTheMainRunLoop] turns it off, and it turns it back on.
var pumpMain atomic.Bool

// TestCMTimeNS pins the Go-duration-to-CMTime conversion, which is the argument
// side of the crossing and needs no media. A nanosecond timescale is what makes
// a seek land where it was asked to: at the customary 600, 7.2501s and 7.25s
// would be the same request.
func TestCMTimeNS(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   time.Duration
	}{
		{"zero", 0},
		{"a second", time.Second},
		{"an odd time a 600 timescale could not hold", 7250123456 * time.Nanosecond},
		{"the probe's duration", 258067 * time.Millisecond},
		{"negative", -time.Second},
	} {
		got := cmTimeNS(tc.in)
		if got.Flags&cmTimeValid == 0 {
			t.Errorf("%s: CMTime is not marked valid; AVFoundation would ignore it", tc.name)
		}
		// Round trip through the same conversion the results come back through.
		if back := got.duration(); back != tc.in {
			t.Errorf("%s: %v -> %+v -> %v, want the duration unchanged", tc.name, tc.in, got, back)
		}
	}
	// The tolerances used for an accurate seek must be a valid ZERO, not an
	// invalid CMTime: an invalid tolerance is ignored and the seek snaps to a
	// keyframe.
	if cmTimeZero.Flags&cmTimeValid == 0 || cmTimeZero.duration() != 0 {
		t.Errorf("cmTimeZero = %+v, want a valid zero", cmTimeZero)
	}
}

// TestOpenPlayerRefusesWhatItCannotPlay covers the rejection paths that need no
// media file, so they run on a CI runner: a path that is not there, a real file
// whose bytes are not a movie, and an empty one. Each must produce a clear error
// rather than a Player that silently never shows anything.
func TestOpenPlayerRefusesWhatItCannotPlay(t *testing.T) {
	runtime.LockOSThread()
	dir := t.TempDir()

	if _, err := OpenPlayer(dir + "/absent.mp4"); err == nil {
		t.Error("OpenPlayer of a nonexistent path succeeded")
	} else {
		t.Logf("absent file: %v", err)
	}

	junk := dir + "/junk.mp4"
	if err := os.WriteFile(junk, []byte("this is not a movie, it is a sentence"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenPlayer(junk); err == nil {
		t.Error("OpenPlayer of a non-movie file succeeded")
	} else {
		t.Logf("junk file: %v", err)
	}

	empty := dir + "/empty.mp4"
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenPlayer(empty); err == nil {
		t.Error("OpenPlayer of an empty file succeeded")
	} else if !errors.Is(err, ErrNoVideoTrack) {
		t.Logf("empty file rejected with: %v", err)
	}

	// And the format refusal, which happens before the file is touched.
	if _, err := OpenPlayer(junk, Options{Format: RGBA}); !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("OpenPlayer with RGBA = %v, want ErrUnsupportedFormat", err)
	}
}

// TestLivePlayer exercises the real AVPlayer path. It needs a video file, so it
// is opt-in via AVFOUNDATION_TEST_FILE.
//
// Everything it asserts is something ONLY a loaded, running player can answer: a
// duration that matches the file, a clock that advances by itself, a seek that
// lands where it was sent, and pictures that come out of the video output. The
// trap it exists to catch is the opposite -- a player that echoes back whatever
// time it was handed while having opened nothing at all.
func TestLivePlayer(t *testing.T) {
	path := os.Getenv("AVFOUNDATION_TEST_FILE")
	if path == "" {
		t.Skip("set AVFOUNDATION_TEST_FILE to a video file to run the live player test")
	}
	// The player and the run loop it is pumped from belong to one thread.
	runtime.LockOSThread()

	p, err := OpenPlayer(path)
	if err != nil {
		t.Fatalf("OpenPlayer(%s) = %v", path, err)
	}
	defer p.Close()

	info := p.Info()
	t.Logf("%dx%d %.3f fps %v", info.Width, info.Height, info.FrameRate, info.Duration)
	if info.Width <= 0 || info.Height <= 0 {
		t.Fatalf("Info reports non-positive dimensions %dx%d", info.Width, info.Height)
	}
	if info.Duration <= 0 {
		t.Fatalf("Info reports duration %v: the item never loaded", info.Duration)
	}
	// The decode-only path is an INDEPENDENT reading of the same file. If the two
	// disagree about the duration, one of them is making it up.
	r, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s) = %v", path, err)
	}
	ri := r.Info()
	r.Close()
	if d := info.Duration - ri.Duration; d > time.Second || d < -time.Second {
		t.Errorf("player says the file is %v long, the reader says %v", info.Duration, ri.Duration)
	}
	if info.Width != ri.Width || info.Height != ri.Height {
		t.Errorf("player says %dx%d, the reader says %dx%d", info.Width, info.Height, ri.Width, ri.Height)
	}

	// Silence: a test suite has no business making noise.
	p.SetVolume(0)

	t.Run("the clock advances by itself", func(t *testing.T) {
		before := p.CurrentTime()
		p.Play()
		start := time.Now()
		p.Pump(time.Second)
		wall := time.Since(start)
		advanced := p.CurrentTime() - before
		p.Pause()
		t.Logf("clock advanced %v over %v of wall time", advanced, wall)
		if advanced < wall/2 {
			t.Errorf("the clock advanced %v over %v: it is not playing", advanced, wall)
		}
		if advanced > wall*2 {
			t.Errorf("the clock advanced %v over %v, which is faster than real time", advanced, wall)
		}
		// Paused means paused: the clock must now stand still.
		held := p.CurrentTime()
		p.Pump(300 * time.Millisecond)
		if moved := p.CurrentTime() - held; moved > 50*time.Millisecond || moved < -50*time.Millisecond {
			t.Errorf("the clock moved %v while paused", moved)
		}
	})

	t.Run("the video output vends pictures", func(t *testing.T) {
		p.Play()
		var (
			frames   int
			nothing  int
			lastPTS  = time.Duration(-1)
			deadline = time.Now().Add(2 * time.Second)
		)
		for time.Now().Before(deadline) && frames < 20 {
			p.Pump(4 * time.Millisecond)
			f, err := p.TryFrame()
			if err != nil {
				t.Fatalf("TryFrame: %v", err)
			}
			if f == nil {
				nothing++
				continue
			}
			if f.Width != info.Width || f.Height != info.Height {
				t.Errorf("frame is %dx%d, the track says %dx%d", f.Width, f.Height, info.Width, info.Height)
			}
			if f.Stride < f.Width*4 {
				t.Errorf("stride %d is less than a %d-pixel row", f.Stride, f.Width)
			}
			if len(f.Pix) < f.Stride*f.Height {
				t.Errorf("Pix is %d bytes, needs %d", len(f.Pix), f.Stride*f.Height)
			}
			if f.Format != BGRA {
				t.Errorf("frame format %v, want BGRA", f.Format)
			}
			if f.PTS < lastPTS {
				t.Errorf("PTS went backwards: %v after %v", f.PTS, lastPTS)
			}
			lastPTS = f.PTS
			// A picture, not a blank buffer: at least one byte somewhere must be
			// set. An all-zero frame is what a decoder that produced nothing looks
			// like.
			nonzero := false
			for i := 0; i < len(f.Pix) && !nonzero; i += 997 {
				nonzero = f.Pix[i] != 0
			}
			if !nonzero {
				t.Error("the frame is entirely zero bytes")
			}
			img := f.ToRGBA(nil)
			if img == nil || img.Rect.Dx() != f.Width {
				t.Errorf("ToRGBA gave %v", img)
			}
			f.Release()
			frames++
		}
		p.Pause()
		t.Logf("%d frames vended, %d polls had nothing new", frames, nothing)
		if frames == 0 {
			t.Fatal("the video output vended no frames at all")
		}
		if nothing == 0 {
			t.Log("note: every poll produced a frame, so the loop was slower than the video")
		}
	})

	t.Run("seeking lands where it was sent", func(t *testing.T) {
		// Times chosen NOT to be keyframes: a player that snapped to the nearest
		// sync sample would miss them by seconds.
		for _, want := range []time.Duration{
			info.Duration / 2,
			info.Duration/4 + 137*time.Millisecond,
			1500 * time.Millisecond,
		} {
			if err := p.Seek(want); err != nil {
				t.Fatalf("Seek(%v) = %v", want, err)
			}
			var got time.Duration
			for i := 0; i < 100; i++ {
				p.Pump(20 * time.Millisecond)
				got = p.CurrentTime()
				if d := got - want; d < 20*time.Millisecond && d > -20*time.Millisecond {
					break
				}
			}
			if d := got - want; d > 20*time.Millisecond || d < -20*time.Millisecond {
				t.Errorf("Seek(%v) landed at %v, off by %v", want, got, d)
			} else {
				t.Logf("Seek(%v) landed at %v (off by %v)", want, got, d)
			}
		}

		// Clamping, checked against the player rather than against the Go code:
		// past the end is the end, before the start is the start.
		if err := p.Seek(100 * info.Duration); err != nil {
			t.Fatal(err)
		}
		p.Pump(500 * time.Millisecond)
		if got := p.CurrentTime(); got > info.Duration+time.Second {
			t.Errorf("seeking past the end landed at %v, beyond the file's %v", got, info.Duration)
		}
		if err := p.Seek(-time.Hour); err != nil {
			t.Fatal(err)
		}
		p.Pump(500 * time.Millisecond)
		if got := p.CurrentTime(); got > 100*time.Millisecond {
			t.Errorf("seeking before the start landed at %v, want the start", got)
		}
	})

	t.Run("rate changes the speed of the clock", func(t *testing.T) {
		if err := p.Seek(0); err != nil {
			t.Fatal(err)
		}
		p.Pump(300 * time.Millisecond)
		before := p.CurrentTime()
		p.SetRate(2)
		start := time.Now()
		p.Pump(time.Second)
		wall := time.Since(start)
		advanced := p.CurrentTime() - before
		p.SetRate(0)
		t.Logf("at rate 2 the clock advanced %v over %v of wall time", advanced, wall)
		if advanced < wall*3/2 {
			t.Errorf("at rate 2 the clock advanced %v over %v, which is not double speed", advanced, wall)
		}
		if p.Playing() {
			t.Error("SetRate(0) left the player reporting itself as playing")
		}
	})

	// A closed player must refuse rather than reach into released AVFoundation
	// objects.
	if err := p.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if _, err := p.TryFrame(); !errors.Is(err, ErrClosed) {
		t.Errorf("TryFrame after Close = %v, want ErrClosed", err)
	}
	if err := p.Seek(0); !errors.Is(err, ErrClosed) {
		t.Errorf("Seek after Close = %v, want ErrClosed", err)
	}
	if got := p.CurrentTime(); got != 0 {
		t.Errorf("CurrentTime after Close = %v, want 0", got)
	}
}

// TestLivePlayerReadyTimeout checks that a player which cannot become ready says
// so instead of hanging. It uses a real file so the failure is the TIMEOUT, not
// a missing track.
func TestLivePlayerReadyTimeout(t *testing.T) {
	path := os.Getenv("AVFOUNDATION_TEST_FILE")
	if path == "" {
		t.Skip("set AVFOUNDATION_TEST_FILE to run the ready-timeout test")
	}
	runtime.LockOSThread()
	// A nanosecond is not long enough for anything to load, so this either
	// returns a ready player (the item was already cached and loaded within the
	// first poll) or the timeout error -- never a hang.
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		p, err := OpenPlayer(path, Options{ReadyTimeout: time.Nanosecond})
		if p != nil {
			p.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Logf("a one-nanosecond timeout gave: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("OpenPlayer with a one-nanosecond timeout hung")
	}
}

// TestLiveLoadNeedsTheMainRunLoop is the control behind this package's loudest
// rule, and the regression test for the trap it exists to keep out of callers'
// hands.
//
// It stops the MAIN thread's run loop -- the one [TestMain] runs, and the one an
// application's event loop is -- and shows that an AVPlayerItem then never
// becomes ready, no matter how long anything sleeps or how hard the calling
// thread's own run loop is pumped. Crucially it also shows the item ANSWERING
// while in that state: seek it, and it reports back the very time it was handed.
// That echo is how a binding that has opened nothing looks exactly like one that
// works, which is why every live assertion here is about something only a loaded
// asset could know.
//
// Then it starts the main run loop again and the same item loads at once.
func TestLiveLoadNeedsTheMainRunLoop(t *testing.T) {
	path := os.Getenv("AVFOUNDATION_TEST_FILE")
	if path == "" {
		t.Skip("set AVFOUNDATION_TEST_FILE to run the main-run-loop control")
	}
	runtime.LockOSThread()
	if err := loadPlayer(); err != nil {
		t.Fatal(err)
	}

	var item, player objc.ID
	objc.AutoreleasePool(func() {
		url := objc.ClassID("NSURL").Send(objc.Sel("fileURLWithPath:"), objc.NSString(path))
		asset := objc.ClassID("AVURLAsset").Send(objc.Sel("URLAssetWithURL:options:"), url, objc.ID(0))
		item = objc.ClassID("AVPlayerItem").Send(objc.Sel("playerItemWithAsset:"), asset)
		player = objc.ClassID("AVPlayer").Send(objc.Sel("playerWithPlayerItem:"), item)
		item.Send(objc.Sel("retain"))
		player.Send(objc.Sel("retain"))
	})
	dp := &darwinPlayer{player: player, item: item}
	defer func() {
		player.Send(objc.Sel("release"))
		item.Send(objc.Sel("release"))
	}()

	// --- main run loop STOPPED -------------------------------------------------
	pumpMain.Store(false)
	defer pumpMain.Store(true)
	// Pump this thread's run loop as hard as waitReady would. It is not the main
	// one, so it buys nothing -- that is the point.
	dead := time.Now().Add(2 * time.Second)
	for time.Now().Before(dead) && objc.Send[int64](item, objc.Sel("status")) != itemStatusReadyToPlay {
		dp.pump(20 * time.Millisecond)
	}
	stoppedStatus := objc.Send[int64](item, objc.Sel("status"))
	stoppedDuration := objc.Send[cmTime](item, objc.Sel("duration")).duration()
	t.Logf("main run loop stopped: after 2s of pumping THIS thread, status %d, duration %v",
		stoppedStatus, stoppedDuration)
	if stoppedStatus == itemStatusReadyToPlay {
		t.Fatal("the item became ready with the main run loop stopped: the rule this " +
			"package documents no longer holds, and the Player docs must be corrected")
	}

	// And the echo: an item that has loaded NOTHING still answers about time.
	const echoAt = 90 * time.Second
	if err := dp.seek(echoAt); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	echoed := dp.currentTime()
	t.Logf("unloaded, it was asked to seek to %v and reports %v -- the value it was given",
		echoAt, echoed)
	if echoed != echoAt {
		t.Logf("note: it did not echo exactly (%v); the trap is real either way", echoed)
	}

	// --- main run loop RUNNING again -------------------------------------------
	pumpMain.Store(true)
	if err := dp.waitReady(path, 10*time.Second); err != nil {
		t.Fatalf("with the main run loop running again the item still did not load: %v", err)
	}
	loaded := objc.Send[cmTime](item, objc.Sel("duration")).duration()
	t.Logf("main run loop running: status %d, duration %v",
		objc.Send[int64](item, objc.Sel("status")), loaded)
	if loaded <= 0 {
		t.Fatal("the item reports no duration even after loading")
	}
	// The duration must be the FILE's, not a zero that happens to look plausible.
	if r, err := Open(path); err == nil {
		ri := r.Info()
		r.Close()
		if d := loaded - ri.Duration; d > time.Second || d < -time.Second {
			t.Errorf("the loaded item says %v, the decoder says %v", loaded, ri.Duration)
		}
	}
}
