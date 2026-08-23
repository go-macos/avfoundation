// Command avplay plays a video file in real time -- with sound -- and reports
// what actually happened: how many frames the video output vended, how far the
// clock moved, and how the two compare to the wall clock.
//
// It is the Player's dogfood and its on-device proof. Everything it does goes
// through the public API, and the numbers it prints are the ones worth
// distrusting: a player whose clock does not advance is not playing, however
// many frames it hands out.
//
// Nothing is displayed. There is no window here; -png writes a frame to disk so
// the picture can be looked at.
package main

import (
	"errors"
	"flag"
	"fmt"
	"image/png"
	"os"
	"runtime"
	"time"

	"github.com/go-macos/avfoundation"
)

func main() {
	// The player must live on the MAIN thread: AVFoundation loads a file through
	// the main dispatch queue, which only the main thread's run loop drains. A
	// player opened anywhere else never becomes ready -- and lies about it.
	runtime.LockOSThread()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "avplay:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		forDur = flag.Duration("for", 5*time.Second, "how long to play before stopping")
		seekTo = flag.Duration("seek", 0, "seek here before playing")
		rate   = flag.Float64("rate", 1, "playback rate: 1 normal, 2 double, -1 backwards")
		volume = flag.Float64("volume", 1, "audio volume, 0 to 1")
		shot   = flag.String("png", "", "write the first frame received to this file")
	)
	flag.Parse()
	if flag.NArg() != 1 {
		return errors.New("usage: avplay [flags] <file>")
	}
	path := flag.Arg(0)

	p, err := avfoundation.OpenPlayer(path)
	if err != nil {
		return err
	}
	defer p.Close()

	i := p.Info()
	fmt.Printf("%s\n", path)
	fmt.Printf("  %dx%d  %.3f fps  %v  %v\n",
		i.Width, i.Height, i.FrameRate, i.Duration.Round(time.Millisecond), p.Format())

	if *seekTo != 0 {
		if err := p.Seek(*seekTo); err != nil {
			return err
		}
		// A seek is asynchronous: the clock only moves once the run loop has run.
		p.Pump(500 * time.Millisecond)
		fmt.Printf("  sought to %v, landed at %v\n",
			*seekTo, p.CurrentTime().Round(time.Millisecond))
	}
	p.SetVolume(*volume)
	p.SetRate(*rate)
	fmt.Printf("  playing at rate %g, volume %g for %v\n", p.Rate(), p.Volume(), *forDur)

	var (
		frames        int
		first, last   time.Duration
		haveFirst     bool
		start         = time.Now()
		startMedia    = p.CurrentTime()
		deadline      = start.Add(*forDur)
		wroteThePNG   = *shot == ""
		blankReturned int
	)
	for time.Now().Before(deadline) {
		// Pump about a frame's worth, then take whatever picture belongs to now.
		p.Pump(4 * time.Millisecond)
		f, err := p.TryFrame()
		if err != nil {
			return err
		}
		if f == nil {
			// No new picture: the poll ran faster than the video's frame rate.
			blankReturned++
			continue
		}
		if !haveFirst {
			first, haveFirst = f.PTS, true
		}
		last = f.PTS
		frames++
		if !wroteThePNG {
			if err := writePNG(*shot, f); err != nil {
				f.Release()
				return err
			}
			fmt.Printf("  wrote %s (%dx%d stride=%d pts=%v)\n",
				*shot, f.Width, f.Height, f.Stride, f.PTS.Round(time.Millisecond))
			wroteThePNG = true
		}
		f.Release()
	}
	wall := time.Since(start)
	p.Pause()
	media := p.CurrentTime() - startMedia

	fmt.Printf("  %d frames vended over %v wall (%.1f fps), %d polls had nothing new\n",
		frames, wall.Round(time.Millisecond), float64(frames)/wall.Seconds(), blankReturned)
	fmt.Printf("  clock moved %v (%.2fx wall), frame PTS %v -> %v\n",
		media.Round(time.Millisecond), media.Seconds()/wall.Seconds(),
		first.Round(time.Millisecond), last.Round(time.Millisecond))
	fmt.Printf("  paused at %v, playing=%v\n",
		p.CurrentTime().Round(time.Millisecond), p.Playing())

	if frames == 0 {
		return errors.New("the video output vended no frames at all")
	}
	return nil
}

func writePNG(name string, f *avfoundation.Frame) error {
	img := f.ToRGBA(nil)
	if img == nil {
		return errors.New("frame released before it could be converted")
	}
	out, err := os.Create(name)
	if err != nil {
		return err
	}
	defer out.Close()
	return png.Encode(out, img)
}
