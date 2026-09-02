//go:build darwin

package avfoundation

import (
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/go-macos/objc"
)

// AVPlayerItemStatus. These are NOT the AVAssetReaderStatus values above:
// failure is 2 here and 3 there, and confusing them turns a broken file into a
// silent success.
const (
	itemStatusUnknown     = 0
	itemStatusReadyToPlay = 1
	itemStatusFailed      = 2
)

// CFRunLoopRunInMode's result codes. Finished means the run loop had nothing to
// run at all, which it reports IMMEDIATELY -- pumping in a loop without noticing
// that turns a wait into a spin on a hot core.
const (
	runLoopFinished = 1
)

// kCFStringEncodingUTF8, for building the run loop mode name. The alternative is
// dlsym'ing kCFRunLoopDefaultMode and dereferencing the result, which is a
// uintptr-to-pointer conversion go vet rightly objects to.
const cfStringEncodingUTF8 = 0x08000100

// cmTimeValid is kCMTimeFlags_Valid. A CMTime without it is "invalid" and every
// API silently ignores it, so a seek built without the flag does nothing at all.
const cmTimeValid = 1

// cmTimeNS builds a CMTime from a Go duration, exactly: a nanosecond timescale
// costs nothing (it fits in the int32 field) and makes the conversion lossless,
// where the customary 600 would round every seek to 1/600 of a second.
func cmTimeNS(d time.Duration) cmTime {
	return cmTime{Value: int64(d), Timescale: int32(time.Second), Flags: cmTimeValid}
}

// cmTimeZero is kCMTimeZero, used as both seek tolerances so a seek lands where
// it was asked to rather than on the nearest keyframe.
var cmTimeZero = cmTime{Value: 0, Timescale: 1, Flags: cmTimeValid}

var (
	cfRunLoopRunInMode        func(mode uintptr, seconds float64, returnAfterSourceHandled bool) int32
	cfStringCreateWithCString func(alloc uintptr, s string, encoding uint32) uintptr

	// runLoopMode is kCFRunLoopDefaultMode, built once and kept for the life of
	// the process.
	runLoopMode uintptr

	loadPlayerOnce sync.Once
	loadPlayerErr  error
)

// loadPlayer resolves what the player needs on top of [load]: the run loop,
// which is the part of CoreFoundation the decode-only path never touches.
func loadPlayer() error {
	loadPlayerOnce.Do(func() { loadPlayerErr = doLoadPlayer() })
	return loadPlayerErr
}

func doLoadPlayer() error {
	if err := load(); err != nil {
		return err
	}
	cf, err := dlopen(objc.CoreFoundation)
	if err != nil {
		return err
	}
	purego.RegisterLibFunc(&cfRunLoopRunInMode, cf, "CFRunLoopRunInMode")
	purego.RegisterLibFunc(&cfStringCreateWithCString, cf, "CFStringCreateWithCString")
	runLoopMode = cfStringCreateWithCString(0, "kCFRunLoopDefaultMode", cfStringEncodingUTF8)
	if runLoopMode == 0 {
		return fmt.Errorf("avfoundation: could not build the run loop mode name")
	}
	return nil
}

// darwinPlayer holds the AVFoundation objects for one open player. All three
// are retained: the convenience constructors hand back autoreleased objects
// that would otherwise die with the pool they were made in.
type darwinPlayer struct {
	player objc.ID
	item   objc.ID
	output objc.ID
}

func init() {
	newPlayerBackend = newDarwinPlayer
}

// newDarwinPlayer builds the AVPlayer, its item and a video output, then waits
// for the item to become playable.
func newDarwinPlayer(path string, opt Options) (playerBackend, Info, error) {
	format, ready := opt.format(), opt.readyTimeout()
	if err := loadPlayer(); err != nil {
		return nil, Info{}, err
	}
	var (
		p    *darwinPlayer
		info Info
		oerr error
	)
	objc.AutoreleasePool(func() {
		url := objc.ClassID("NSURL").Send(objc.Sel("fileURLWithPath:"), objc.NSString(path))
		if url == 0 {
			oerr = &OpenError{Path: path, Stage: "NSURL fileURLWithPath:"}
			return
		}
		asset := objc.ClassID("AVURLAsset").Send(
			objc.Sel("URLAssetWithURL:options:"), url, objc.ID(0))
		if asset == 0 {
			oerr = &OpenError{Path: path, Stage: "AVURLAsset URLAssetWithURL:"}
			return
		}
		// A file with no video track has nothing for the video output to vend.
		// Refusing here is the same answer [Open] gives, from the same question.
		tracks := asset.Send(objc.Sel("tracksWithMediaType:"), objc.NSString("vide"))
		if tracks == 0 || objc.Send[uint64](tracks, objc.Sel("count")) == 0 {
			oerr = fmt.Errorf("%w: %s", ErrNoVideoTrack, path)
			return
		}
		track := objc.Send[objc.ID](tracks, objc.Sel("objectAtIndex:"), uint64(0))
		size := objc.Send[cgSize](track, objc.Sel("naturalSize"))
		info = Info{
			Width:     int(size.Width),
			Height:    int(size.Height),
			FrameRate: float64(objc.Send[float32](track, objc.Sel("nominalFrameRate"))),
			Duration:  objc.Send[cmTime](asset, objc.Sel("duration")).duration(),
		}

		item := objc.ClassID("AVPlayerItem").Send(objc.Sel("playerItemWithAsset:"), asset)
		if item == 0 {
			oerr = &OpenError{Path: path, Stage: "AVPlayerItem playerItemWithAsset:"}
			return
		}
		// pixelBufferAttributes = @{ @"PixelFormatType": @(format) }, the same key
		// the reader's output settings use.
		num := objc.ClassID("NSNumber").Send(
			objc.Sel("numberWithUnsignedInt:"), uint32(format))
		attrs := objc.ClassID("NSDictionary").Send(
			objc.Sel("dictionaryWithObject:forKey:"), num, objc.NSString("PixelFormatType"))
		output := objc.ClassID("AVPlayerItemVideoOutput").Send(objc.Sel("alloc")).
			Send(objc.Sel("initWithPixelBufferAttributes:"), attrs)
		if output == 0 {
			oerr = &OpenError{Path: path, Stage: "AVPlayerItemVideoOutput"}
			return
		}
		item.Send(objc.Sel("addOutput:"), output)

		player := objc.ClassID("AVPlayer").Send(objc.Sel("playerWithPlayerItem:"), item)
		if player == 0 {
			output.Send(objc.Sel("release"))
			oerr = &OpenError{Path: path, Stage: "AVPlayer playerWithPlayerItem:"}
			return
		}
		if uid := opt.AudioDeviceUID; uid != "" {
			// Where the sound goes. Without this it goes to the system default,
			// which on a machine playing to a headset is the machine's own
			// speakers -- silently, because nothing failed.
			player.Send(objc.Sel("setAudioOutputDeviceUniqueID:"), objc.NSString(uid))
		}
		player.Send(objc.Sel("retain"))
		item.Send(objc.Sel("retain"))
		// output came from alloc/init and is already owned.
		p = &darwinPlayer{player: player, item: item, output: output}
	})
	if oerr != nil {
		return nil, Info{}, oerr
	}

	// An AVPlayerItem loads ASYNCHRONOUSLY, on the run loop. Until it is ready
	// the player answers every question with a plausible-looking zero -- duration
	// 0, time 0 -- and a caller that does not wait here would be told its file
	// is empty rather than that it has not loaded yet.
	if err := p.waitReady(path, ready); err != nil {
		p.close()
		return nil, Info{}, err
	}
	// The item knows the duration more precisely than the asset did before it
	// loaded; prefer it when it says anything at all.
	if d := objc.Send[cmTime](p.item, objc.Sel("duration")).duration(); d > 0 {
		info.Duration = d
	}
	return p, info, nil
}

// waitReady pumps the run loop until the item is playable, or gives up.
func (p *darwinPlayer) waitReady(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		st := objc.Send[int64](p.item, objc.Sel("status"))
		switch st {
		case itemStatusReadyToPlay:
			return nil
		case itemStatusFailed:
			return &OpenError{Path: path, Stage: "AVPlayerItem load", Detail: p.itemError()}
		}
		if !time.Now().Before(deadline) {
			// Still itemStatusUnknown. Report the status rather than guessing --
			// a timeout and a failure are different problems -- and name the
			// cause that is almost always behind this one, because "status 0"
			// on its own sends people looking at their file.
			return &OpenError{
				Path:  path,
				Stage: "AVPlayerItem load",
				Detail: fmt.Sprintf(
					"still not ready after %v (status %d); an AVPlayerItem loads through the MAIN queue, "+
						"so the main thread's run loop must be running -- see the Player docs",
					timeout, st),
			}
		}
		p.pump(10 * time.Millisecond)
	}
}

// itemError reports whatever AVFoundation said about a failed item.
func (p *darwinPlayer) itemError() string {
	err := objc.Send[objc.ID](p.item, objc.Sel("error"))
	if err == 0 {
		return ""
	}
	return objc.GoString(objc.Send[objc.ID](err, objc.Sel("localizedDescription")))
}

func (p *darwinPlayer) play()  { p.player.Send(objc.Sel("play")) }
func (p *darwinPlayer) pause() { p.player.Send(objc.Sel("pause")) }

// setRate goes through -[AVPlayer setRate:], which takes a C float. Setting a
// non-zero rate starts playback; setting 0 pauses.
func (p *darwinPlayer) setRate(rate float64) {
	p.player.Send(objc.Sel("setRate:"), float32(rate))
}

func (p *darwinPlayer) setVolume(v float64) {
	p.player.Send(objc.Sel("setVolume:"), float32(v))
}

func (p *darwinPlayer) currentTime() time.Duration {
	return objc.Send[cmTime](p.player, objc.Sel("currentTime")).duration()
}

// seek asks for an EXACT time. -[AVPlayer seekToTime:] alone snaps to the
// nearest sync sample -- measured landing at 58.333s for a request of 60s --
// which is not what someone dragging a scrubber means.
func (p *darwinPlayer) seek(at time.Duration) error {
	p.player.Send(objc.Sel("seekToTime:toleranceBefore:toleranceAfter:"),
		cmTimeNS(at), cmTimeZero, cmTimeZero)
	return nil
}

// tryFrame asks the video output for the picture belonging to the player's
// current time, and reports (nil, nil) when there is no new one.
func (p *darwinPlayer) tryFrame(format PixelFormat) (*Frame, error) {
	now := objc.Send[cmTime](p.player, objc.Sel("currentTime"))
	if !objc.Send[bool](p.output, objc.Sel("hasNewPixelBufferForItemTime:"), now) {
		return nil, nil
	}
	// The second argument is an optional CMTime out-parameter for the display
	// time; NULL says the caller does not want it.
	pb := objc.Send[uintptr](p.output,
		objc.Sel("copyPixelBufferForItemTime:itemTimeForDisplay:"), now, uintptr(0))
	if pb == 0 {
		// Not an error: the output can change its mind between the two calls, and
		// the honest answer is still "no new picture".
		return nil, nil
	}
	if r := cvPixelBufferLockBaseAddress(pb, cvReadOnly); r != 0 {
		cfRelease(pb)
		return nil, fmt.Errorf("avfoundation: CVPixelBufferLockBaseAddress: %d", r)
	}
	base := cvPixelBufferGetBaseAddress(pb)
	w := int(cvPixelBufferGetWidth(pb))
	h := int(cvPixelBufferGetHeight(pb))
	stride := int(cvPixelBufferGetBytesPerRow(pb))
	if base == nil || w <= 0 || h <= 0 || stride <= 0 {
		cvPixelBufferUnlockBaseAddress(pb, cvReadOnly)
		cfRelease(pb)
		return nil, fmt.Errorf("avfoundation: empty pixel buffer %dx%d stride %d", w, h, stride)
	}
	f := &Frame{
		Width:  w,
		Height: h,
		Stride: stride,
		Format: format,
		// The player's clock IS this frame's presentation time: the output was
		// asked for the picture belonging to that instant.
		PTS: now.duration(),
		Pix: unsafe.Slice((*byte)(base), stride*h),
	}
	// Unlike the reader's, this buffer is owned outright -- copyPixelBuffer...
	// returns it retained -- so it is unlocked and released together.
	f.release = func() {
		cvPixelBufferUnlockBaseAddress(pb, cvReadOnly)
		cfRelease(pb)
	}
	return f, nil
}

// pump runs THIS thread's run loop for about d. CoreFoundation offers no other
// kind: a run loop can only be run by the thread that owns it, which is why the
// player has to live on the main thread (see the [Player] documentation).
func (p *darwinPlayer) pump(d time.Duration) {
	if d <= 0 {
		cfRunLoopRunInMode(runLoopMode, 0, true)
		return
	}
	deadline := time.Now().Add(d)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		// In slices, so a caller pumping for a long time still returns near its
		// deadline rather than at the mercy of one long blocking call.
		slice := min(remaining, 10*time.Millisecond)
		if cfRunLoopRunInMode(runLoopMode, slice.Seconds(), false) == runLoopFinished {
			// Nothing was scheduled, so the call returned at once. Wait out the
			// slice rather than spinning on it.
			time.Sleep(slice)
		}
	}
}

func (p *darwinPlayer) close() error {
	if p == nil || p.player == 0 {
		return nil
	}
	// Pause first: releasing a playing AVPlayer leaves its audio running until
	// the object is actually collected.
	p.player.Send(objc.Sel("pause"))
	p.item.Send(objc.Sel("removeOutput:"), p.output)
	p.output.Send(objc.Sel("release"))
	p.item.Send(objc.Sel("release"))
	p.player.Send(objc.Sel("release"))
	p.player, p.item, p.output = 0, 0, 0
	return nil
}
