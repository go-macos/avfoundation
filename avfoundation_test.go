package avfoundation

import (
	"errors"
	"image"
	"io"
	"strings"
	"testing"
	"time"
)

// withSeams swaps the platform seams for fakes and restores them afterwards, so
// these tests run identically on darwin (where init() wired AVFoundation) and on
// any other platform.
func withSeams(t *testing.T,
	open func(string, PixelFormat) (handle, Info, error),
	next func(handle, PixelFormat) (*Frame, error),
	closeFn func(handle) error,
) {
	t.Helper()
	o, n, c := openFile, nextFrame, closeFile
	t.Cleanup(func() { openFile, nextFrame, closeFile = o, n, c })
	if open != nil {
		openFile = open
	}
	if next != nil {
		nextFrame = next
	}
	if closeFn != nil {
		closeFile = closeFn
	}
}

func TestOpenErrorError(t *testing.T) {
	bare := (&OpenError{Path: "/a/b.mp4", Stage: "startReading"}).Error()
	if !strings.Contains(bare, "/a/b.mp4") || !strings.Contains(bare, "startReading") {
		t.Errorf("Error() = %q, want it to name the path and stage", bare)
	}
	if strings.Contains(bare, "failed:") {
		t.Errorf("Error() = %q, want no colon-detail when Detail is empty", bare)
	}
	withDetail := (&OpenError{Path: "p", Stage: "s", Detail: "status 3"}).Error()
	if !strings.Contains(withDetail, "status 3") {
		t.Errorf("Error() = %q, want it to carry the detail", withDetail)
	}
}

func TestPixelFormatString(t *testing.T) {
	if got := BGRA.String(); got != "BGRA" {
		t.Errorf("BGRA.String() = %q, want %q", got, "BGRA")
	}
	if got := RGBA.String(); got != "RGBA" {
		t.Errorf("RGBA.String() = %q, want %q", got, "RGBA")
	}
	// A code with a non-printable byte must not be rendered as mojibake.
	if got := PixelFormat(0x00000001).String(); !strings.HasPrefix(got, "PixelFormat(") {
		t.Errorf("non-printable format rendered as %q, want the numeric form", got)
	}
}

func TestOptionsFormatDefaultsToBGRA(t *testing.T) {
	if got := (Options{}).format(); got != BGRA {
		t.Errorf("zero Options asked for %v, want BGRA (what the decoder produces natively)", got)
	}
	if got := (Options{Format: RGBA}).format(); got != RGBA {
		t.Errorf("explicit format = %v, want RGBA", got)
	}
}

func TestFrameRelease(t *testing.T) {
	var f *Frame
	f.Release() // must not panic on a nil frame
	if !f.Released() {
		t.Error("a nil frame should report itself released")
	}

	released := 0
	g := &Frame{Pix: []byte{1, 2, 3, 4}, release: func() { released++ }}
	if g.Released() {
		t.Error("a fresh frame reports itself released")
	}
	g.Release()
	if released != 1 || !g.Released() || g.Pix != nil {
		t.Errorf("after Release: calls=%d released=%v pix=%v", released, g.Released(), g.Pix)
	}
	g.Release() // idempotent: must not hand the buffer back twice
	if released != 1 {
		t.Errorf("Release called the platform release %d times, want 1", released)
	}

	// A frame with no release function must still mark itself released.
	h := &Frame{Pix: []byte{9}}
	h.Release()
	if !h.Released() {
		t.Error("a frame without a release func did not mark itself released")
	}
}

// bgraFrame builds a 2x2 frame whose stride is deliberately LARGER than
// width*4, because that is the real case: decoders pad rows, and indexing by
// width instead of stride shears the picture progressively down the frame.
func bgraFrame(format PixelFormat) *Frame {
	const w, h, stride = 2, 2, 12 // 12 > 2*4, so there is padding
	pix := make([]byte, stride*h)
	// Row 0: two pixels, B,G,R,A. First is pure blue in BGRA, second pure red.
	copy(pix[0:], []byte{255, 0, 0, 0, 0, 0, 255, 0})
	// Row 1: green, then white.
	copy(pix[stride:], []byte{0, 255, 0, 0, 255, 255, 255, 0})
	return &Frame{Width: w, Height: h, Stride: stride, Format: format, Pix: pix}
}

func TestFrameToRGBASwapsBGRA(t *testing.T) {
	f := bgraFrame(BGRA)
	img := f.ToRGBA(nil)
	if img == nil {
		t.Fatal("ToRGBA returned nil for a live frame")
	}
	if img.Rect.Dx() != 2 || img.Rect.Dy() != 2 {
		t.Fatalf("image is %v, want 2x2", img.Rect)
	}
	for _, tc := range []struct {
		x, y       int
		r, g, b, a uint8
	}{
		{0, 0, 0, 0, 255, 255},     // BGRA blue -> RGBA blue
		{1, 0, 255, 0, 0, 255},     // BGRA red -> RGBA red
		{0, 1, 0, 255, 0, 255},     // green
		{1, 1, 255, 255, 255, 255}, // white
	} {
		i := img.PixOffset(tc.x, tc.y)
		got := [4]uint8{img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3]}
		want := [4]uint8{tc.r, tc.g, tc.b, tc.a}
		if got != want {
			t.Errorf("pixel (%d,%d) = %v, want %v", tc.x, tc.y, got, want)
		}
	}
}

// TestFrameToRGBAForcesOpaque is the guard against an invisible picture: the
// source alpha bytes above are all ZERO, as some decoders leave them.
func TestFrameToRGBAForcesOpaque(t *testing.T) {
	for _, format := range []PixelFormat{BGRA, RGBA} {
		img := bgraFrame(format).ToRGBA(nil)
		for y := 0; y < 2; y++ {
			for x := 0; x < 2; x++ {
				if a := img.Pix[img.PixOffset(x, y)+3]; a != 0xff && format == BGRA {
					t.Errorf("%v (%d,%d) alpha = %d, want 255", format, x, y, a)
				}
			}
		}
	}
}

func TestFrameToRGBACopiesRGBAUnchanged(t *testing.T) {
	f := bgraFrame(RGBA)
	img := f.ToRGBA(nil)
	// No swap: the first pixel's bytes come through in source order.
	i := img.PixOffset(0, 0)
	if got := [4]uint8{img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3]}; got != [4]uint8{255, 0, 0, 0} {
		t.Errorf("RGBA source pixel = %v, want the bytes unchanged", got)
	}
}

func TestFrameToRGBAReusesDst(t *testing.T) {
	f := bgraFrame(BGRA)
	dst := image.NewRGBA(image.Rect(0, 0, 2, 2))
	if got := f.ToRGBA(dst); got != dst {
		t.Error("ToRGBA allocated a new image despite being given one of the right size")
	}
	// The wrong size must be replaced, not written past.
	small := image.NewRGBA(image.Rect(0, 0, 1, 1))
	if got := f.ToRGBA(small); got == small {
		t.Error("ToRGBA reused an image of the wrong size")
	}
}

func TestFrameToRGBARefusesWhatItCannotRead(t *testing.T) {
	var nilFrame *Frame
	if nilFrame.ToRGBA(nil) != nil {
		t.Error("ToRGBA of a nil frame returned an image")
	}
	rel := bgraFrame(BGRA)
	rel.Release()
	if rel.ToRGBA(nil) != nil {
		t.Error("ToRGBA of a released frame returned an image; its pixels are gone")
	}
	for _, f := range []*Frame{
		{Width: 0, Height: 2, Stride: 8},
		{Width: 2, Height: 0, Stride: 8},
		{Width: -1, Height: 2, Stride: 8},
	} {
		if f.ToRGBA(nil) != nil {
			t.Errorf("ToRGBA of %+v returned an image", f)
		}
	}
}

func TestOpenPropagatesFailure(t *testing.T) {
	sentinel := errors.New("nope")
	withSeams(t, func(string, PixelFormat) (handle, Info, error) {
		return nil, Info{}, sentinel
	}, nil, nil)
	if _, err := Open("/x.mp4"); !errors.Is(err, sentinel) {
		t.Errorf("Open() = %v, want the platform error", err)
	}
}

func TestOpenPassesTheRequestedFormat(t *testing.T) {
	var got PixelFormat
	withSeams(t, func(_ string, f PixelFormat) (handle, Info, error) {
		got = f
		return "h", Info{Width: 4, Height: 2, FrameRate: 25, Duration: time.Second}, nil
	}, nil, nil)

	r, err := Open("/x.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if got != BGRA {
		t.Errorf("default asked the platform for %v, want BGRA", got)
	}
	if r.Format() != BGRA {
		t.Errorf("Format() = %v, want BGRA", r.Format())
	}
	if i := r.Info(); i.Width != 4 || i.Height != 2 || i.FrameRate != 25 || i.Duration != time.Second {
		t.Errorf("Info() = %+v, want the platform's answer", i)
	}

	// An explicit BGRA is passed through unchanged.
	got = 0
	if _, err := Open("/x.mp4", Options{Format: BGRA}); err != nil {
		t.Fatal(err)
	}
	if got != BGRA {
		t.Errorf("explicit option asked the platform for %v, want BGRA", got)
	}

	// A format the decoder will not produce must be refused BEFORE the platform
	// is asked, so the caller gets a reason instead of an opaque reader status.
	got = 0
	if _, err := Open("/x.mp4", Options{Format: RGBA}); !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("Open with RGBA = %v, want ErrUnsupportedFormat", err)
	}
	if got != 0 {
		t.Errorf("the platform was asked for %v despite the format being unsupported", got)
	}
}

func TestReaderNextFrameAndClose(t *testing.T) {
	closes := 0
	frames := []*Frame{{Width: 1, Height: 1, PTS: 0}, {Width: 1, Height: 1, PTS: time.Second}}
	i := 0
	withSeams(t,
		func(string, PixelFormat) (handle, Info, error) { return "h", Info{}, nil },
		func(handle, PixelFormat) (*Frame, error) {
			if i >= len(frames) {
				return nil, io.EOF
			}
			f := frames[i]
			i++
			return f, nil
		},
		func(handle) error { closes++; return nil },
	)

	r, err := Open("/x.mp4")
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 2; n++ {
		f, err := r.NextFrame()
		if err != nil {
			t.Fatalf("frame %d: %v", n, err)
		}
		if f != frames[n] {
			t.Errorf("frame %d is not the one the platform produced", n)
		}
	}
	if _, err := r.NextFrame(); !errors.Is(err, io.EOF) {
		t.Errorf("past the end: %v, want io.EOF", err)
	}

	if err := r.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	if err := r.Close(); err != nil { // idempotent
		t.Fatalf("second Close() = %v", err)
	}
	if closes != 1 {
		t.Errorf("platform close called %d times, want 1", closes)
	}
	// A closed reader must refuse to decode rather than touch a freed decoder.
	if _, err := r.NextFrame(); !errors.Is(err, ErrClosed) {
		t.Errorf("NextFrame after Close = %v, want ErrClosed", err)
	}
}

func TestReaderCloseReportsPlatformError(t *testing.T) {
	sentinel := errors.New("teardown failed")
	withSeams(t,
		func(string, PixelFormat) (handle, Info, error) { return "h", Info{}, nil },
		nil,
		func(handle) error { return sentinel },
	)
	r, err := Open("/x.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); !errors.Is(err, sentinel) {
		t.Errorf("Close() = %v, want the platform error", err)
	}
}
