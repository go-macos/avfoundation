package avfoundation

import (
	"errors"
	"math"
	"testing"
	"time"
)

// fakeBackend records what the portable layer asked the platform to do, so the
// clamping, the closed-player guards and the rate bookkeeping can be checked
// without a Mac -- and, on a Mac, without touching AVFoundation.
type fakeBackend struct {
	plays, pauses, closes int
	rates                 []float64
	volumes               []float64
	seeks                 []time.Duration
	pumps                 []time.Duration
	now                   time.Duration
	frame                 *Frame
	frameFormat           PixelFormat
	seekErr               error
	frameErr              error
	closeErr              error
}

func (f *fakeBackend) play()                      { f.plays++ }
func (f *fakeBackend) pause()                     { f.pauses++ }
func (f *fakeBackend) setRate(r float64)          { f.rates = append(f.rates, r) }
func (f *fakeBackend) setVolume(v float64)        { f.volumes = append(f.volumes, v) }
func (f *fakeBackend) currentTime() time.Duration { return f.now }
func (f *fakeBackend) seek(at time.Duration) error {
	f.seeks = append(f.seeks, at)
	return f.seekErr
}
func (f *fakeBackend) tryFrame(p PixelFormat) (*Frame, error) {
	f.frameFormat = p
	return f.frame, f.frameErr
}
func (f *fakeBackend) pump(d time.Duration) { f.pumps = append(f.pumps, d) }
func (f *fakeBackend) close() error         { f.closes++; return f.closeErr }

// withPlayerSeam swaps the platform constructor for a fake and restores it
// afterwards, so these tests run identically on darwin and everywhere else.
func withPlayerSeam(t *testing.T, open func(string, PixelFormat, time.Duration) (playerBackend, Info, error)) {
	t.Helper()
	prev := newPlayerBackend
	t.Cleanup(func() { newPlayerBackend = prev })
	newPlayerBackend = open
}

// openFake wires a fake backend and opens a player on it.
func openFake(t *testing.T, info Info, opts ...Options) (*Player, *fakeBackend) {
	t.Helper()
	b := &fakeBackend{}
	withPlayerSeam(t, func(string, PixelFormat, time.Duration) (playerBackend, Info, error) {
		return b, info, nil
	})
	p, err := OpenPlayer("/x.mp4", opts...)
	if err != nil {
		t.Fatalf("OpenPlayer: %v", err)
	}
	return p, b
}

func TestOptionsReadyTimeout(t *testing.T) {
	if got := (Options{}).readyTimeout(); got != 10*time.Second {
		t.Errorf("zero ReadyTimeout = %v, want ten seconds", got)
	}
	if got := (Options{ReadyTimeout: -1}).readyTimeout(); got != 10*time.Second {
		t.Errorf("negative ReadyTimeout = %v, want the default", got)
	}
	if got := (Options{ReadyTimeout: time.Minute}).readyTimeout(); got != time.Minute {
		t.Errorf("explicit ReadyTimeout = %v, want a minute", got)
	}
}

func TestOpenPlayerPassesFormatAndTimeout(t *testing.T) {
	var (
		gotPath   string
		gotFormat PixelFormat
		gotReady  time.Duration
	)
	info := Info{Width: 1280, Height: 720, FrameRate: 30, Duration: 258067 * time.Millisecond}
	withPlayerSeam(t, func(path string, f PixelFormat, ready time.Duration) (playerBackend, Info, error) {
		gotPath, gotFormat, gotReady = path, f, ready
		return &fakeBackend{}, info, nil
	})

	p, err := OpenPlayer("/movie.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/movie.mp4" || gotFormat != BGRA || gotReady != 10*time.Second {
		t.Errorf("platform asked for (%q, %v, %v), want (/movie.mp4, BGRA, 10s)", gotPath, gotFormat, gotReady)
	}
	if p.Info() != info {
		t.Errorf("Info() = %+v, want the platform's answer %+v", p.Info(), info)
	}
	if p.Format() != BGRA {
		t.Errorf("Format() = %v, want BGRA", p.Format())
	}
	if p.Duration() != info.Duration {
		t.Errorf("Duration() = %v, want %v", p.Duration(), info.Duration)
	}
	// A fresh player is paused, at full volume: it must not start making noise
	// before it is told to.
	if p.Playing() || p.Rate() != 0 {
		t.Errorf("a fresh player reports playing=%v rate=%v, want paused", p.Playing(), p.Rate())
	}
	if p.Volume() != 1 {
		t.Errorf("Volume() = %v, want 1", p.Volume())
	}

	// Options travel through.
	if _, err := OpenPlayer("/movie.mp4", Options{Format: BGRA, ReadyTimeout: 2 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if gotFormat != BGRA || gotReady != 2*time.Second {
		t.Errorf("explicit options gave (%v, %v), want (BGRA, 2s)", gotFormat, gotReady)
	}
}

func TestOpenPlayerRefusesUndecodableFormat(t *testing.T) {
	asked := false
	withPlayerSeam(t, func(string, PixelFormat, time.Duration) (playerBackend, Info, error) {
		asked = true
		return &fakeBackend{}, Info{}, nil
	})
	// RGBA makes the decoder fail rather than convert, so it is refused BEFORE
	// the platform is asked -- the caller gets a reason, not a status code.
	if _, err := OpenPlayer("/x.mp4", Options{Format: RGBA}); !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("OpenPlayer with RGBA = %v, want ErrUnsupportedFormat", err)
	}
	if asked {
		t.Error("the platform was asked for a format it cannot produce")
	}
}

func TestOpenPlayerPropagatesFailure(t *testing.T) {
	sentinel := errors.New("item never loaded")
	withPlayerSeam(t, func(string, PixelFormat, time.Duration) (playerBackend, Info, error) {
		return nil, Info{}, sentinel
	})
	if _, err := OpenPlayer("/x.mp4"); !errors.Is(err, sentinel) {
		t.Errorf("OpenPlayer = %v, want the platform error", err)
	}
}

func TestPlayPauseTrackTheRate(t *testing.T) {
	p, b := openFake(t, Info{Duration: time.Minute})

	p.Play()
	if b.plays != 1 || p.Rate() != 1 || !p.Playing() {
		t.Errorf("after Play: plays=%d rate=%v playing=%v", b.plays, p.Rate(), p.Playing())
	}
	p.Pause()
	if b.pauses != 1 || p.Rate() != 0 || p.Playing() {
		t.Errorf("after Pause: pauses=%d rate=%v playing=%v", b.pauses, p.Rate(), p.Playing())
	}
	// Play does NOT restore a rate set earlier -- it sets 1, exactly as
	// -[AVPlayer play] does.
	p.SetRate(2.5)
	p.Pause()
	p.Play()
	if p.Rate() != 1 {
		t.Errorf("Play after SetRate(2.5) left rate %v, want 1", p.Rate())
	}
}

func TestSetRate(t *testing.T) {
	p, b := openFake(t, Info{Duration: time.Minute})

	p.SetRate(2)
	if p.Rate() != 2 || !p.Playing() || len(b.rates) != 1 || b.rates[0] != 2 {
		t.Errorf("SetRate(2): rate=%v playing=%v platform=%v", p.Rate(), p.Playing(), b.rates)
	}
	p.SetRate(-1) // backwards is a legitimate rate
	if p.Rate() != -1 || !p.Playing() {
		t.Errorf("SetRate(-1): rate=%v playing=%v", p.Rate(), p.Playing())
	}
	p.SetRate(0)
	if p.Rate() != 0 || p.Playing() {
		t.Errorf("SetRate(0): rate=%v playing=%v, want paused", p.Rate(), p.Playing())
	}

	// A rate that is not a number must never reach AVFoundation: it would take
	// it and leave a clock nobody can reason about.
	sent := len(b.rates)
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		p.SetRate(bad)
		if p.Rate() != 0 {
			t.Errorf("SetRate(%v) changed the rate to %v", bad, p.Rate())
		}
	}
	if len(b.rates) != sent {
		t.Errorf("the platform was sent %d extra rates, want none", len(b.rates)-sent)
	}
}

func TestSetVolumeClamps(t *testing.T) {
	p, b := openFake(t, Info{})

	for _, tc := range []struct{ in, want float64 }{
		{0.5, 0.5},
		{-1, 0}, // below the range AVPlayer accepts
		{7, 1},  // above it
		{0, 0},
		{1, 1},
	} {
		p.SetVolume(tc.in)
		if p.Volume() != tc.want {
			t.Errorf("SetVolume(%v) -> Volume() = %v, want %v", tc.in, p.Volume(), tc.want)
		}
		if got := b.volumes[len(b.volumes)-1]; got != tc.want {
			t.Errorf("SetVolume(%v) sent %v to the platform, want %v", tc.in, got, tc.want)
		}
	}

	sent := len(b.volumes)
	p.SetVolume(math.NaN())
	if p.Volume() != 1 || len(b.volumes) != sent {
		t.Errorf("SetVolume(NaN) changed the volume to %v (platform calls %d)", p.Volume(), len(b.volumes))
	}
}

func TestCurrentTime(t *testing.T) {
	p, b := openFake(t, Info{Duration: time.Minute})
	b.now = 42 * time.Second
	if got := p.CurrentTime(); got != 42*time.Second {
		t.Errorf("CurrentTime() = %v, want the platform's clock", got)
	}
	// A closed player has no clock to report and must not call into a released
	// AVPlayer to find one.
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if got := p.CurrentTime(); got != 0 {
		t.Errorf("CurrentTime() after Close = %v, want 0", got)
	}
}

func TestSeekClampsToTheFile(t *testing.T) {
	p, b := openFake(t, Info{Duration: 100 * time.Second})

	for _, tc := range []struct{ ask, want time.Duration }{
		{30 * time.Second, 30 * time.Second},
		{-5 * time.Second, 0},                  // before the start is the start
		{500 * time.Second, 100 * time.Second}, // past the end is the end
		{100 * time.Second, 100 * time.Second}, // exactly the end is allowed
	} {
		if err := p.Seek(tc.ask); err != nil {
			t.Fatalf("Seek(%v) = %v", tc.ask, err)
		}
		if got := b.seeks[len(b.seeks)-1]; got != tc.want {
			t.Errorf("Seek(%v) asked the platform for %v, want %v", tc.ask, got, tc.want)
		}
	}

	// A file of unknown duration -- a stream -- must not have its seeks clamped
	// to zero length.
	q, qb := openFake(t, Info{})
	if err := q.Seek(9 * time.Second); err != nil {
		t.Fatal(err)
	}
	if qb.seeks[0] != 9*time.Second {
		t.Errorf("with no known duration, Seek asked for %v, want 9s", qb.seeks[0])
	}
}

func TestSeekReportsErrors(t *testing.T) {
	p, b := openFake(t, Info{Duration: time.Minute})
	sentinel := errors.New("seek refused")
	b.seekErr = sentinel
	if err := p.Seek(time.Second); !errors.Is(err, sentinel) {
		t.Errorf("Seek = %v, want the platform error", err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Seek(time.Second); !errors.Is(err, ErrClosed) {
		t.Errorf("Seek after Close = %v, want ErrClosed", err)
	}
}

func TestTryFrame(t *testing.T) {
	p, b := openFake(t, Info{Duration: time.Minute})

	// Nothing new is the common case and is NOT an error: a display loop polls
	// faster than the video's frame rate.
	f, err := p.TryFrame()
	if f != nil || err != nil {
		t.Errorf("TryFrame with nothing new = (%v, %v), want (nil, nil)", f, err)
	}
	if b.frameFormat != BGRA {
		t.Errorf("the platform was asked for format %v, want BGRA", b.frameFormat)
	}

	want := &Frame{Width: 4, Height: 2, Stride: 16, Format: BGRA, PTS: time.Second}
	b.frame = want
	if got, err := p.TryFrame(); got != want || err != nil {
		t.Errorf("TryFrame = (%v, %v), want the platform's frame", got, err)
	}

	sentinel := errors.New("pixel buffer would not lock")
	b.frame, b.frameErr = nil, sentinel
	if _, err := p.TryFrame(); !errors.Is(err, sentinel) {
		t.Errorf("TryFrame = %v, want the platform error", err)
	}

	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.TryFrame(); !errors.Is(err, ErrClosed) {
		t.Errorf("TryFrame after Close = %v, want ErrClosed", err)
	}
}

func TestPump(t *testing.T) {
	p, b := openFake(t, Info{})
	p.Pump(5 * time.Millisecond)
	p.Pump(0)
	if len(b.pumps) != 2 || b.pumps[0] != 5*time.Millisecond || b.pumps[1] != 0 {
		t.Errorf("pumps = %v, want [5ms 0]", b.pumps)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	// Pumping a closed player must not run a run loop on behalf of a released
	// AVPlayer.
	p.Pump(time.Second)
	if len(b.pumps) != 2 {
		t.Errorf("Pump after Close reached the platform: %v", b.pumps)
	}
}

func TestCloseIsIdempotentAndSilencesThePlayer(t *testing.T) {
	p, b := openFake(t, Info{Duration: time.Minute})
	p.Play()
	if err := p.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	if b.closes != 1 {
		t.Errorf("platform close called %d times, want 1", b.closes)
	}
	if p.Playing() || p.Rate() != 0 {
		t.Errorf("a closed player reports playing=%v rate=%v", p.Playing(), p.Rate())
	}
	// Every command must become a no-op rather than reaching a released object.
	plays, pauses, rates, volumes := b.plays, b.pauses, len(b.rates), len(b.volumes)
	p.Play()
	p.Pause()
	p.SetRate(2)
	p.SetVolume(0.5)
	if b.plays != plays || b.pauses != pauses || len(b.rates) != rates || len(b.volumes) != volumes {
		t.Error("a command on a closed player reached the platform")
	}
	if p.Rate() != 0 || p.Volume() != 1 {
		t.Errorf("a closed player's state moved: rate=%v volume=%v", p.Rate(), p.Volume())
	}
}

func TestCloseReportsPlatformError(t *testing.T) {
	p, b := openFake(t, Info{})
	sentinel := errors.New("teardown failed")
	b.closeErr = sentinel
	if err := p.Close(); !errors.Is(err, sentinel) {
		t.Errorf("Close = %v, want the platform error", err)
	}
}
