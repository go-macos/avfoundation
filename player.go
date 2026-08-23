package avfoundation

import (
	"fmt"
	"math"
	"time"
)

// Player plays a file in real time: with sound, and with a clock that can be
// paused, moved and run at a different speed.
//
// It is the second of this package's two paths, and the two are not
// interchangeable. [Reader] decodes as fast as the hardware will go and hands
// every frame over in order — the right shape for transcoding, analysis, or a
// renderer that owns its own clock. Player wraps AVPlayer, which owns the clock
// itself: it renders audio through the system's output, drops video frames to
// stay in sync, and answers questions about where in the file it is. Nothing in
// [Reader] can do any of that, and nothing here decodes at 2000 frames a second.
//
// Video comes out through [Player.TryFrame], which is a POLL, not a pull:
// AVPlayerItemVideoOutput vends at most one buffer per item time, and asking
// again for the same instant answers nothing. That is the API AVFoundation
// offers and it is the right one for a display loop, which draws when the screen
// is ready and simply wants whatever picture belongs to now.
//
// # The main thread, and the run loop
//
// A Player must be opened and driven from the process's MAIN thread, with that
// thread's run loop running. This is not a style preference; it is measured.
//
// AVFoundation loads a file through the main dispatch queue, and only the main
// thread's run loop drains that queue. An AVPlayerItem in a program whose main
// thread is parked never becomes ready: it sits at status 0 for as long as you
// like, however hard some other thread's run loop is pumped, and on a cold file
// it reports a duration of 0 with it. Worse, a player in that state still
// ANSWERS: seek it to ninety seconds and it reports ninety seconds — the value
// it was just handed. That echo is exactly how a binding that has opened nothing
// can be mistaken for one that works, and it is what the tests here are built to
// catch. Sleeping is not waiting.
//
// So: runtime.LockOSThread on the main goroutine, open the player there, and
// drive it from there. An application already does this — the main thread's run
// loop IS the window system's event loop, and a Player used from a go-widgets or
// AppKit program needs nothing extra. A program with no event loop of its own
// must run one, which is what [Player.Pump] is for; [OpenPlayer] runs it for the
// duration of the load and returns an error rather than a half-loaded Player.
//
// Once loaded, the player keeps going on AVFoundation's own queues. Measured:
// the clock advances, seeks complete and frames come out with nothing but
// time.Sleep between the calls. Pump is still what a headless loop should wait
// with — it costs no more than sleeping and it is where run loop work gets done
// — but it is not the engine.
//
// A Player is NOT safe for concurrent use.
type Player struct {
	info   Info
	format PixelFormat
	closed bool
	// rate is the last rate asked for, mirroring AVPlayer's own: 0 while paused,
	// 1 while playing normally, negative while playing backwards.
	rate   float64
	volume float64

	b playerBackend
}

// OpenPlayer opens path for real-time playback and waits for it to become
// playable, which needs the main thread's run loop and therefore happens here
// rather than leaving a half-loaded Player in the caller's hands. Call it from
// the main thread; see [Player] for why.
//
// The player starts PAUSED at the beginning of the file, at full volume. Call
// [Player.Play] to start it.
func OpenPlayer(path string, opts ...Options) (*Player, error) {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	format := o.format()
	if !decodable[format] {
		// Refused here, for the same reason [Open] refuses it: AVFoundation's own
		// answer to an impossible pixel format is an opaque status code.
		return nil, fmt.Errorf("%w: %v (only %v is decodable)", ErrUnsupportedFormat, format, BGRA)
	}
	b, info, err := newPlayerBackend(path, format, o.readyTimeout())
	if err != nil {
		return nil, err
	}
	return &Player{info: info, format: format, volume: 1, b: b}, nil
}

// Info returns the video track's description.
func (p *Player) Info() Info { return p.info }

// Format returns the pixel format frames are decoded into.
func (p *Player) Format() PixelFormat { return p.format }

// Duration returns the file's duration, which is [Info]'s.
func (p *Player) Duration() time.Duration { return p.info.Duration }

// Play starts playback at normal speed, with sound.
//
// Like AVPlayer's own play, it sets the rate to 1 — a rate set earlier with
// [Player.SetRate] is NOT restored. Use SetRate to resume at another speed.
func (p *Player) Play() {
	if p.closed {
		return
	}
	p.rate = 1
	p.b.play()
}

// Pause stops playback where it is, keeping the position. It is [Player.Play]'s
// inverse and leaves the rate at 0.
func (p *Player) Pause() {
	if p.closed {
		return
	}
	p.rate = 0
	p.b.pause()
}

// Playing reports whether the clock is running, which is exactly whether the
// rate is not zero.
func (p *Player) Playing() bool { return !p.closed && p.rate != 0 }

// Rate returns the current playback rate: 0 paused, 1 normal, 2 twice as fast,
// negative for backwards.
func (p *Player) Rate() float64 { return p.rate }

// SetRate sets the playback speed. A rate of 0 pauses; 1 is normal speed; 2 is
// twice as fast; a negative rate plays backwards, which not every file supports
// — measured working on H.264 in MP4.
//
// A NaN or infinite rate is ignored rather than handed to AVFoundation, which
// would take it and produce a clock that cannot be reasoned about.
func (p *Player) SetRate(rate float64) {
	if p.closed || math.IsNaN(rate) || math.IsInf(rate, 0) {
		return
	}
	p.rate = rate
	p.b.setRate(rate)
}

// Volume returns the audio volume, from 0 to 1.
func (p *Player) Volume() float64 { return p.volume }

// SetVolume sets the audio volume, clamped to the 0..1 AVPlayer accepts. NaN is
// ignored. This is the player's own volume, not the system's.
func (p *Player) SetVolume(v float64) {
	if p.closed || math.IsNaN(v) {
		return
	}
	v = min(max(v, 0), 1)
	p.volume = v
	p.b.setVolume(v)
}

// CurrentTime returns where the player is in the file.
//
// It is the player's clock, not a frame's timestamp: while playing it advances
// continuously, and the frame [Player.TryFrame] hands back is the one that
// belongs to it. A closed player reports 0.
func (p *Player) CurrentTime() time.Duration {
	if p.closed {
		return 0
	}
	return p.b.currentTime()
}

// Seek moves playback to at, accurately: the request goes to AVFoundation with
// zero tolerance, so it lands on the time asked for rather than on the nearest
// keyframe. That costs decoding from the previous keyframe forward, which is the
// trade a viewer wants — measured landing within a microsecond of the request.
//
// The time is clamped to the file: before the start becomes the start, past the
// end becomes the end. Seeking is asynchronous; the run loop must run (see
// [Player.Pump]) before [Player.CurrentTime] reports the new position.
func (p *Player) Seek(at time.Duration) error {
	if p.closed {
		return ErrClosed
	}
	if at < 0 {
		at = 0
	}
	if d := p.info.Duration; d > 0 && at > d {
		at = d
	}
	return p.b.seek(at)
}

// TryFrame returns the frame for the player's current time, or (nil, nil) when
// there is no NEW one to give.
//
// The nil-nil answer is not an error and is the common case: a display loop runs
// faster than the video's frame rate, and AVPlayerItemVideoOutput vends a buffer
// only when the picture has changed. A caller draws the last frame again, or
// nothing.
//
// It is TryFrame rather than NextFrame because there is no "next": the output
// answers about a moment in time, not about a position in a stream, and after a
// seek or a rate change the moment can go backwards. A blocking NextFrame would
// have to either spin or run the run loop behind the caller's back, and both
// are worse than telling the truth.
//
// Every frame returned must be released, once — see [Frame].
func (p *Player) TryFrame() (*Frame, error) {
	if p.closed {
		return nil, ErrClosed
	}
	return p.b.tryFrame(p.format)
}

// Pump runs the calling thread's run loop for about d.
//
// It is what a program with no event loop of its own — a command-line tool, a
// test — should wait with instead of time.Sleep, and it must be called from the
// MAIN thread: a run loop can only be run by the thread that owns it, and the
// main one is the one AVFoundation needs (see above).
//
// An application whose window system already runs the main run loop must NOT
// call it: running a run loop that is already being run from underneath itself
// invites reentrancy.
//
// A d of zero or less runs one pass and returns, which is the non-blocking form.
func (p *Player) Pump(d time.Duration) {
	if p.closed {
		return
	}
	p.b.pump(d)
}

// Close stops playback and releases the player. Frames already handed out stay
// valid until they are individually released.
func (p *Player) Close() error {
	if p.closed {
		return nil
	}
	p.closed = true
	p.rate = 0
	return p.b.close()
}

// ---------------------------------------------------------------------------
// Platform seam. Unlike [Reader]'s three free functions, a player is a
// long-lived object with eight operations on it, so the seam is one constructor
// and an interface — same idea, less repetition. The darwin build assigns the
// AVPlayer implementation in an init(); every other platform assigns a stub that
// reports [ErrUnsupported].
// ---------------------------------------------------------------------------

// playerBackend is one platform's live player. Every method is called only
// while the Player is open, so none of them has to defend against use after
// close.
type playerBackend interface {
	play()
	pause()
	setRate(rate float64)
	setVolume(v float64)
	currentTime() time.Duration
	seek(at time.Duration) error
	tryFrame(f PixelFormat) (*Frame, error)
	pump(d time.Duration)
	close() error
}

// newPlayerBackend opens path and waits up to ready for it to become playable.
var newPlayerBackend func(path string, f PixelFormat, ready time.Duration) (playerBackend, Info, error)
