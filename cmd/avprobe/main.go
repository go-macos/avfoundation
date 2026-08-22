// Command avprobe decodes the first frames of a video file and writes them as
// PNGs. It is this package's dogfood and its on-device proof: everything it does
// goes through the public API.
package main

import (
	"errors"
	"flag"
	"fmt"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/go-macos/avfoundation"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "avprobe:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		count = flag.Int("n", 3, "how many frames to decode")
		write = flag.Bool("png", true, "write each decoded frame as a PNG")
		outes = flag.String("out", ".", "directory to write PNGs into")
		all   = flag.Bool("all", false, "decode the whole track and report the rate (writes nothing)")
	)
	flag.Parse()
	if flag.NArg() != 1 {
		return errors.New("usage: avprobe [flags] <file>")
	}
	path := flag.Arg(0)

	r, err := avfoundation.Open(path)
	if err != nil {
		return err
	}
	defer r.Close()

	i := r.Info()
	fmt.Printf("%s\n", path)
	fmt.Printf("  %dx%d  %.3f fps  %v  decoding into %s\n",
		i.Width, i.Height, i.FrameRate, i.Duration.Round(time.Millisecond), r.Format())

	if *all {
		n, start := 0, time.Now()
		var lastPTS time.Duration
		for {
			f, err := r.NextFrame()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			lastPTS = f.PTS
			f.Release()
			n++
		}
		el := time.Since(start)
		fmt.Printf("  decoded %d frames in %v (%.1f fps, %.1fx real time)\n",
			n, el.Round(time.Millisecond), float64(n)/el.Seconds(),
			lastPTS.Seconds()/el.Seconds())
		fmt.Printf("  last PTS %v vs track duration %v\n",
			lastPTS.Round(time.Millisecond), i.Duration.Round(time.Millisecond))
		return nil
	}

	for n := 0; n < *count; n++ {
		f, err := r.NextFrame()
		if errors.Is(err, io.EOF) {
			fmt.Printf("  end of track after %d frames\n", n)
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Printf("  frame %d: %dx%d stride=%d pts=%v\n",
			n, f.Width, f.Height, f.Stride, f.PTS.Round(time.Microsecond))
		if *write {
			name := filepath.Join(*outes, fmt.Sprintf("frame%02d.png", n))
			if err := writePNG(name, f); err != nil {
				f.Release()
				return err
			}
			fmt.Printf("           wrote %s\n", name)
		}
		f.Release()
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
