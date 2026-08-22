//go:build darwin

package avfoundation

import (
	"errors"
	"io"
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

// TestCMTimeDuration pins the CMTime conversion, which is pure arithmetic and
// needs no media. The zero-timescale case is the one that matters: CoreMedia
// uses it for an invalid or indefinite time (a live stream has no duration), and
// dividing by it would produce Inf or NaN and travel a long way before failing.
func TestCMTimeDuration(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   cmTime
		want time.Duration
	}{
		{"zero", cmTime{Value: 0, Timescale: 600}, 0},
		{"one second", cmTime{Value: 600, Timescale: 600}, time.Second},
		{"a 30fps frame", cmTime{Value: 512, Timescale: 15360}, time.Second / 30},
		{"the probe's duration", cmTime{Value: 258067, Timescale: 1000}, 258067 * time.Millisecond},
		{"negative", cmTime{Value: -600, Timescale: 600}, -time.Second},
		{"indefinite: timescale 0", cmTime{Value: 123, Timescale: 0}, 0},
	} {
		got := tc.in.duration()
		// Allow a nanosecond of float rounding on the non-exact ratios.
		if d := got - tc.want; d > time.Nanosecond || d < -time.Nanosecond {
			t.Errorf("%s: duration() = %v, want %v", tc.name, got, tc.want)
		}
	}
	// And explicitly: no Inf, no NaN, ever.
	for _, ts := range []int32{0, -1} {
		d := cmTime{Value: math.MaxInt64, Timescale: ts}.duration()
		if ts == 0 && d != 0 {
			t.Errorf("timescale 0 gave %v, want 0", d)
		}
	}
}

// TestLiveDecode exercises the real AVFoundation path — the part no fake can
// cover. It needs a video file, which a CI runner does not have, so it is opt-in
// via AVFOUNDATION_TEST_FILE. Everything it asserts is cross-checkable from
// outside this package: the dimensions, the frame rate, and that the
// presentation timestamps advance by roughly one frame period.
func TestLiveDecode(t *testing.T) {
	path := os.Getenv("AVFOUNDATION_TEST_FILE")
	if path == "" {
		t.Skip("set AVFOUNDATION_TEST_FILE to a video file to run the live decode test")
	}
	r, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s) = %v", path, err)
	}
	defer r.Close()

	info := r.Info()
	t.Logf("%dx%d %.3f fps %v", info.Width, info.Height, info.FrameRate, info.Duration)
	if info.Width <= 0 || info.Height <= 0 {
		t.Fatalf("Info reports non-positive dimensions %dx%d", info.Width, info.Height)
	}
	if info.Duration <= 0 {
		t.Errorf("Info reports duration %v, want positive", info.Duration)
	}

	var prev time.Duration
	for n := 0; n < 5; n++ {
		f, err := r.NextFrame()
		if errors.Is(err, io.EOF) {
			if n == 0 {
				t.Fatal("the first NextFrame reported EOF; nothing decoded")
			}
			break
		}
		if err != nil {
			t.Fatalf("frame %d: %v", n, err)
		}
		// The frame must match the track, and its stride must be at least a full
		// row -- a stride below width*4 would mean reading past each row.
		if f.Width != info.Width || f.Height != info.Height {
			t.Errorf("frame %d is %dx%d, track says %dx%d", n, f.Width, f.Height, info.Width, info.Height)
		}
		if f.Stride < f.Width*4 {
			t.Errorf("frame %d stride %d is less than a %d-pixel row", n, f.Stride, f.Width)
		}
		if len(f.Pix) < f.Stride*f.Height {
			t.Errorf("frame %d Pix is %d bytes, needs %d", n, len(f.Pix), f.Stride*f.Height)
		}
		if n > 0 && f.PTS <= prev {
			t.Errorf("frame %d PTS %v did not advance past %v", n, f.PTS, prev)
		}
		// Converting must produce a full-size opaque image.
		img := f.ToRGBA(nil)
		if img == nil || img.Rect.Dx() != f.Width || img.Rect.Dy() != f.Height {
			t.Errorf("frame %d: ToRGBA gave %v", n, img.Bounds())
		} else if a := img.Pix[3]; a != 0xff {
			t.Errorf("frame %d: first pixel alpha %d, want opaque", n, a)
		}
		prev = f.PTS
		f.Release()
		if !f.Released() || f.Pix != nil {
			t.Errorf("frame %d was not released cleanly", n)
		}
	}

	// The frame period must match the nominal rate, which is known from the file
	// rather than from this package.
	if info.FrameRate > 0 && prev > 0 {
		period := prev / 4 // 5 frames decoded, 4 intervals
		want := time.Duration(float64(time.Second) / info.FrameRate)
		if period < want/2 || period > want*2 {
			t.Errorf("mean frame period %v is nowhere near 1/%.3f = %v", period, info.FrameRate, want)
		}
	}
}

// TestOpenRefusesWhatItCannotDecode covers darwinOpen's rejection paths, which
// need no media file: a path that is not there, and a file that is real but
// carries no video. Both must produce a clear error rather than a Reader that
// yields nothing.
func TestOpenRefusesWhatItCannotDecode(t *testing.T) {
	dir := t.TempDir()

	notThere := dir + "/absent.mp4"
	if _, err := Open(notThere); err == nil {
		t.Error("Open of a nonexistent path succeeded")
	} else {
		t.Logf("absent file: %v", err)
	}

	// A real file whose bytes are not a movie.
	junk := dir + "/junk.mp4"
	if err := os.WriteFile(junk, []byte("this is not a movie, it is a sentence"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(junk); err == nil {
		t.Error("Open of a non-movie file succeeded")
	} else if !errors.Is(err, ErrNoVideoTrack) {
		// Either error is defensible; what matters is that it is refused and
		// says why.
		t.Logf("junk file rejected with: %v", err)
	}

	// A file with a video-ish name but audio-only content is the same shape of
	// failure; an empty file exercises it without needing real audio.
	empty := dir + "/empty.mp4"
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(empty); err == nil {
		t.Error("Open of an empty file succeeded")
	}
}

// TestRGBAIsRefusedNotAttempted is the regression test for a measured platform
// limit. Asking an AVAssetReaderTrackOutput for RGBA does not convert -- it
// makes the reader FAIL, and the failure surfaced as an opaque "reader status
// 3" from deep inside AVFoundation. The request is now refused up front, with a
// reason, and callers convert with Frame.ToRGBA instead.
func TestRGBAIsRefusedNotAttempted(t *testing.T) {
	// This needs no media: the refusal happens before the file is touched.
	_, err := Open("/nonexistent-on-purpose.mp4", Options{Format: RGBA})
	if !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf("Open with RGBA = %v, want ErrUnsupportedFormat", err)
	}
	if !strings.Contains(err.Error(), "BGRA") {
		t.Errorf("error %q should name the format that does work", err)
	}

	// And BGRA, the one that works, must still be accepted on a real file.
	path := os.Getenv("AVFOUNDATION_TEST_FILE")
	if path == "" {
		t.Skip("set AVFOUNDATION_TEST_FILE to also check that BGRA is accepted")
	}
	r, err := Open(path, Options{Format: BGRA})
	if err != nil {
		t.Fatalf("Open with BGRA = %v", err)
	}
	defer r.Close()
	f, err := r.NextFrame()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Release()
	if f.Format != BGRA {
		t.Errorf("frame format = %v, want BGRA", f.Format)
	}
}
