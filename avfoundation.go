// Package avfoundation decodes video files on macOS through AVFoundation, with
// no cgo.
//
// It exists because there is no realistic pure-Go alternative for the job: a
// software H.264 or HEVC decoder in Go will not keep up with 4K at 60 frames a
// second, while the hardware decoder in every Mac will do it without warming up.
// AVFoundation is the system's own front door to that decoder, and it brings
// demuxing and format support along with it. Everything here goes through
// github.com/ebitengine/purego, so a consumer still builds with
// CGO_ENABLED=0 — the constraint the fleet actually cares about is no cgo, not
// no operating system.
//
// The model is a pull: [Open] a file and call [Reader.NextFrame] until it
// reports io.EOF. Frames come out as fast as they decode, carrying their
// presentation timestamps, and the caller owns the clock. That is the right
// shape for a renderer that has its own frame loop — an immersive viewer must
// draw when the display is ready, not when a player decides.
//
// Frames are NOT copied. A [Frame] holds the decoder's own buffer locked, and
// its pixels stay valid until [Frame.Release]. At 4K that saves about 33 MB of
// copying per frame, which is the difference between comfortable and not.
package avfoundation

import (
	"errors"
	"fmt"
	"image"
	"time"
)

// Errors reported by the package. They are stable and may be tested with
// errors.Is.
var (
	// ErrUnsupported is returned by every entry point on non-darwin platforms.
	ErrUnsupported = errors.New("avfoundation: unsupported on this platform (darwin only)")
	// ErrNoVideoTrack is returned by [Open] for a file with no video in it.
	ErrNoVideoTrack = errors.New("avfoundation: file has no video track")
	// ErrClosed is returned when a Reader is used after [Reader.Close].
	ErrClosed = errors.New("avfoundation: reader is closed")
	// ErrReleased is returned by [Frame] accessors after [Frame.Release].
	ErrReleased = errors.New("avfoundation: frame has been released")
	// ErrUnsupportedFormat is returned by [Open] for a pixel format the decoder
	// will not produce. See [Options.Format].
	ErrUnsupportedFormat = errors.New("avfoundation: the decoder will not produce that pixel format")
)

// OpenError describes a failure to open or start reading a file, carrying
// whatever AVFoundation said about it.
type OpenError struct {
	Path   string
	Stage  string
	Detail string
}

func (e *OpenError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("avfoundation: %s: %s failed", e.Path, e.Stage)
	}
	return fmt.Sprintf("avfoundation: %s: %s failed: %s", e.Path, e.Stage, e.Detail)
}

// PixelFormat is a CoreVideo pixel format, which is a four-character code.
type PixelFormat uint32

// The formats this package can ask the decoder for.
const (
	// BGRA is 32-bit BGRA, 8 bits per channel. It is what the display pipeline
	// and Metal both prefer, so it is the default: asking for RGBA instead would
	// make something, somewhere, swap two bytes per pixel for nothing.
	BGRA PixelFormat = 0x42475241 // 'BGRA'
	// RGBA is 32-bit RGBA. It DESCRIBES a frame's layout but is NOT accepted as a
	// decode request: measured on macOS, an AVAssetReaderTrackOutput asked for
	// RGBA fails outright (reader status 3) rather than converting. Use
	// [Frame.ToRGBA] to convert a decoded BGRA frame instead.
	RGBA PixelFormat = 0x52474241 // 'RGBA'
)

// decodable lists the formats the decoder will actually produce. It is a list of
// ONE because that is what was measured, not because the others were not
// considered: RGBA makes the reader fail, and the planar YUV formats the
// hardware prefers need a multi-plane Frame this package does not have yet.
var decodable = map[PixelFormat]bool{BGRA: true}

// String renders the format as its four-character code.
func (f PixelFormat) String() string {
	b := [4]byte{byte(f >> 24), byte(f >> 16), byte(f >> 8), byte(f)}
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return fmt.Sprintf("PixelFormat(%#08x)", uint32(f))
		}
	}
	return string(b[:])
}

// Info describes a file's video track, read when it is opened.
type Info struct {
	// Width and Height are the coded dimensions in pixels.
	Width, Height int
	// FrameRate is the track's nominal rate in frames per second. It is nominal:
	// variable-frame-rate material reports an average, so time anything that
	// matters by a frame's own PTS rather than by counting frames.
	FrameRate float64
	// Duration is the track's duration.
	Duration time.Duration
}

// Frame is one decoded frame. Its pixels alias the decoder's buffer and are
// valid until [Frame.Release] — which the caller must call, once, for every
// frame it receives. Holding many unreleased frames will stall the decoder,
// because it is waiting for its own buffers back.
type Frame struct {
	// Width and Height are this frame's dimensions in pixels.
	Width, Height int
	// Stride is the number of bytes per row, which is USUALLY more than
	// Width*4: the decoder pads rows for alignment. Indexing by Width*4 instead
	// of Stride produces a picture that shears progressively down the frame,
	// which looks like a decode bug and is not one.
	Stride int
	// Format is the pixel layout, as requested when the reader was opened.
	Format PixelFormat
	// PTS is the presentation timestamp: when this frame should be shown,
	// relative to the start of the file.
	PTS time.Duration
	// Pix is the frame's bytes, Stride*Height of them.
	Pix []byte

	released bool
	release  func()
}

// Release hands the buffer back to the decoder. It is safe to call more than
// once, and a released Frame's Pix must not be read.
func (f *Frame) Release() {
	if f == nil || f.released {
		return
	}
	f.released = true
	f.Pix = nil
	if f.release != nil {
		f.release()
	}
}

// Released reports whether the frame's buffer has been handed back.
func (f *Frame) Released() bool { return f == nil || f.released }

// ToRGBA copies the frame into an *image.RGBA, converting from BGRA if needed.
//
// dst is reused when it is exactly the right size, so a render loop can hold one
// image and not allocate per frame; pass nil, or an image of the wrong size, to
// get a fresh one. It returns nil for a released frame.
func (f *Frame) ToRGBA(dst *image.RGBA) *image.RGBA {
	if f == nil || f.released || f.Width <= 0 || f.Height <= 0 {
		return nil
	}
	if dst == nil || dst.Rect.Dx() != f.Width || dst.Rect.Dy() != f.Height {
		dst = image.NewRGBA(image.Rect(0, 0, f.Width, f.Height))
	}
	swap := f.Format == BGRA
	for y := 0; y < f.Height; y++ {
		srcRow := f.Pix[y*f.Stride : y*f.Stride+f.Width*4]
		dstRow := dst.Pix[y*dst.Stride : y*dst.Stride+f.Width*4]
		if !swap {
			copy(dstRow, srcRow)
			continue
		}
		for x := 0; x < f.Width*4; x += 4 {
			// BGRA -> RGBA. Alpha is forced opaque: a decoded video frame has no
			// meaningful alpha, and some decoders leave the byte at zero, which
			// would render the whole picture invisible.
			dstRow[x+0] = srcRow[x+2]
			dstRow[x+1] = srcRow[x+1]
			dstRow[x+2] = srcRow[x+0]
			dstRow[x+3] = 0xff
		}
	}
	return dst
}

// Options parametrise [Open]. The zero value asks for BGRA, which is what the
// decoder produces natively.
type Options struct {
	// Format is the pixel format to decode into. Zero means [BGRA].
	Format PixelFormat
}

// format resolves the requested format, defaulting to BGRA.
func (o Options) format() PixelFormat {
	if o.Format == 0 {
		return BGRA
	}
	return o.Format
}

// Reader decodes a file's first video track, in order, from the beginning.
//
// It is NOT safe for concurrent use: one goroutine pulls frames. Seeking is not
// implemented yet — a reader plays through once.
type Reader struct {
	info   Info
	format PixelFormat
	closed bool

	// h is the platform handle; nil on an unsupported platform.
	h handle
}

// Info returns the video track's description.
func (r *Reader) Info() Info { return r.info }

// Format returns the pixel format frames are decoded into.
func (r *Reader) Format() PixelFormat { return r.format }

// Open opens path and prepares its first video track for decoding.
func Open(path string, opts ...Options) (*Reader, error) {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	format := o.format()
	if !decodable[format] {
		// Refuse here, with a reason. Passing an unsupported format through would
		// surface as an opaque "reader status 3" from deep inside AVFoundation.
		return nil, fmt.Errorf("%w: %v (only %v is decodable)", ErrUnsupportedFormat, format, BGRA)
	}
	h, info, err := openFile(path, format)
	if err != nil {
		return nil, err
	}
	return &Reader{info: info, format: format, h: h}, nil
}

// NextFrame decodes and returns the next frame, or io.EOF when the track is
// exhausted. The caller must [Frame.Release] every frame it receives.
func (r *Reader) NextFrame() (*Frame, error) {
	if r.closed {
		return nil, ErrClosed
	}
	return nextFrame(r.h, r.format)
}

// Close releases the decoder. Frames already handed out stay valid until they
// are individually released.
func (r *Reader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	return closeFile(r.h)
}

// ---------------------------------------------------------------------------
// Platform seams. The darwin build assigns the real AVFoundation
// implementations in an init(); every other platform assigns unsupported stubs.
// Keeping the portable logic above them lets this file be exercised without a
// Mac, and lets a test drive Reader through fakes.
// ---------------------------------------------------------------------------

// handle is the platform's decoder state, opaque to the portable layer.
type handle any

var (
	// openFile opens path and reports its video track.
	openFile func(path string, f PixelFormat) (handle, Info, error)
	// nextFrame pulls the next decoded frame, or io.EOF at the end.
	nextFrame func(h handle, f PixelFormat) (*Frame, error)
	// closeFile tears the decoder down.
	closeFile func(h handle) error
)
