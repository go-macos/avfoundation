//go:build darwin

package avfoundation

import (
	"fmt"
	"io"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/go-macos/objc"
)

// Framework paths. AVFoundation carries the reader; CoreMedia the sample
// buffers and their timestamps; CoreVideo the pixel buffers.
const (
	frameworkAVFoundation = "/System/Library/Frameworks/AVFoundation.framework/AVFoundation"
	frameworkCoreMedia    = "/System/Library/Frameworks/CoreMedia.framework/CoreMedia"
	frameworkCoreVideo    = "/System/Library/Frameworks/CoreVideo.framework/CoreVideo"
)

// AVAssetReaderStatus.
const (
	statusUnknown   = 0
	statusReading   = 1
	statusCompleted = 2
	statusFailed    = 3
	statusCancelled = 4
)

// cvReadOnly is the CVPixelBufferLockFlags bit for a read-only lock. Locking
// read-only lets the decoder keep the buffer in whatever memory it likes; a
// read-write lock can force a copy out of GPU-visible memory.
const cvReadOnly = 1

// cmTime is CoreMedia's CMTime: 24 bytes, passed and returned BY VALUE. On arm64
// a struct this size is returned indirectly through a hidden pointer, so this
// crossing is the one worth distrusting — it is checked on-device against a
// duration and frame rate known from outside this package.
type cmTime struct {
	Value     int64
	Timescale int32
	Flags     uint32
	Epoch     int64
}

// duration converts to a Go duration. A zero timescale means the time is
// invalid or indefinite, which is not an error: a live stream has no duration.
func (t cmTime) duration() time.Duration {
	if t.Timescale == 0 {
		return 0
	}
	return time.Duration(float64(t.Value) / float64(t.Timescale) * float64(time.Second))
}

// cgSize is CoreGraphics' CGSize, used for a track's natural size.
type cgSize struct{ Width, Height float64 }

var (
	cmSampleBufferGetImageBuffer func(uintptr) uintptr
	cmSampleBufferGetPTS         func(uintptr) cmTime
	cfRelease                    func(uintptr)

	cvPixelBufferLockBaseAddress   func(uintptr, uint64) int32
	cvPixelBufferUnlockBaseAddress func(uintptr, uint64) int32
	cvPixelBufferGetBaseAddress    func(uintptr) unsafe.Pointer
	cvPixelBufferGetBytesPerRow    func(uintptr) uint64
	cvPixelBufferGetWidth          func(uintptr) uint64
	cvPixelBufferGetHeight         func(uintptr) uint64
)

var (
	loadOnce sync.Once
	loadErr  error
)

// load resolves the frameworks and C entry points once.
func load() error {
	loadOnce.Do(func() { loadErr = doLoad() })
	return loadErr
}

func doLoad() error {
	if err := objc.Load(objc.Foundation, frameworkAVFoundation,
		frameworkCoreMedia, frameworkCoreVideo); err != nil {
		return err
	}
	cm, err := dlopen(frameworkCoreMedia)
	if err != nil {
		return err
	}
	cv, err := dlopen(frameworkCoreVideo)
	if err != nil {
		return err
	}
	cf, err := dlopen(objc.CoreFoundation)
	if err != nil {
		return err
	}
	purego.RegisterLibFunc(&cmSampleBufferGetImageBuffer, cm, "CMSampleBufferGetImageBuffer")
	purego.RegisterLibFunc(&cmSampleBufferGetPTS, cm, "CMSampleBufferGetPresentationTimeStamp")
	purego.RegisterLibFunc(&cfRelease, cf, "CFRelease")
	purego.RegisterLibFunc(&cvPixelBufferLockBaseAddress, cv, "CVPixelBufferLockBaseAddress")
	purego.RegisterLibFunc(&cvPixelBufferUnlockBaseAddress, cv, "CVPixelBufferUnlockBaseAddress")
	purego.RegisterLibFunc(&cvPixelBufferGetBaseAddress, cv, "CVPixelBufferGetBaseAddress")
	purego.RegisterLibFunc(&cvPixelBufferGetBytesPerRow, cv, "CVPixelBufferGetBytesPerRow")
	purego.RegisterLibFunc(&cvPixelBufferGetWidth, cv, "CVPixelBufferGetWidth")
	purego.RegisterLibFunc(&cvPixelBufferGetHeight, cv, "CVPixelBufferGetHeight")
	return nil
}

// dlopen is a seam so a test can force doLoad's failure path.
var dlopen = func(path string) (uintptr, error) {
	return purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
}

// darwinReader holds the AVFoundation objects for one open file. They are
// retained, because everything AVFoundation hands back from a convenience
// constructor is autoreleased and would otherwise die with the pool.
type darwinReader struct {
	reader objc.ID
	output objc.ID
}

func init() {
	openFile = darwinOpen
	nextFrame = darwinNextFrame
	closeFile = darwinClose
}

// darwinOpen builds the asset, reader and BGRA-or-RGBA output, and starts
// reading.
func darwinOpen(path string, format PixelFormat) (handle, Info, error) {
	if err := load(); err != nil {
		return nil, Info{}, err
	}
	var (
		h    *darwinReader
		info Info
		oerr error
	)
	// Every convenience constructor below returns an autoreleased object, and a
	// Go program has no pool of its own. Without one they accumulate for the
	// lifetime of the process.
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

		// AVMediaTypeVideo is the string @"vide". Building it directly avoids a
		// Dlsym on the exported constant, and the uintptr->pointer conversion
		// that go vet's unsafeptr check would rightly flag.
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

		reader := objc.ClassID("AVAssetReader").Send(
			objc.Sel("assetReaderWithAsset:error:"), asset, objc.ID(0))
		if reader == 0 {
			oerr = &OpenError{Path: path, Stage: "AVAssetReader assetReaderWithAsset:"}
			return
		}

		// outputSettings = @{ @"PixelFormatType": @(format) }. "PixelFormatType"
		// IS the underlying string of kCVPixelBufferPixelFormatTypeKey.
		num := objc.ClassID("NSNumber").Send(
			objc.Sel("numberWithUnsignedInt:"), uint32(format))
		settings := objc.ClassID("NSDictionary").Send(
			objc.Sel("dictionaryWithObject:forKey:"), num, objc.NSString("PixelFormatType"))

		output := objc.ClassID("AVAssetReaderTrackOutput").Send(
			objc.Sel("assetReaderTrackOutputWithTrack:outputSettings:"), track, settings)
		if output == 0 {
			oerr = &OpenError{Path: path, Stage: "AVAssetReaderTrackOutput"}
			return
		}
		reader.Send(objc.Sel("addOutput:"), output)
		if !objc.Send[bool](reader, objc.Sel("startReading")) {
			oerr = &OpenError{
				Path:  path,
				Stage: "startReading",
				Detail: fmt.Sprintf("reader status %d",
					objc.Send[int64](reader, objc.Sel("status"))),
			}
			return
		}
		reader.Send(objc.Sel("retain"))
		output.Send(objc.Sel("retain"))
		h = &darwinReader{reader: reader, output: output}
	})
	if oerr != nil {
		return nil, Info{}, oerr
	}
	return h, info, nil
}

// darwinNextFrame pulls one sample buffer and wraps its pixels, without copying
// them.
func darwinNextFrame(h handle, format PixelFormat) (*Frame, error) {
	dr, ok := h.(*darwinReader)
	if !ok || dr == nil {
		return nil, ErrClosed
	}
	sbuf := objc.Send[uintptr](dr.output, objc.Sel("copyNextSampleBuffer"))
	if sbuf == 0 {
		// No buffer means either a clean end or a failure, and the two must not
		// be confused: a caller that treats a decode failure as end-of-file
		// silently truncates the video.
		switch st := objc.Send[int64](dr.reader, objc.Sel("status")); st {
		case statusCompleted:
			return nil, io.EOF
		case statusReading:
			// Reading but nothing to give: the track ended without the reader
			// having transitioned yet.
			return nil, io.EOF
		default:
			return nil, fmt.Errorf("avfoundation: decode stopped, reader status %d", st)
		}
	}
	pb := cmSampleBufferGetImageBuffer(sbuf)
	if pb == 0 {
		cfRelease(sbuf)
		return nil, fmt.Errorf("avfoundation: sample buffer carries no image")
	}
	if r := cvPixelBufferLockBaseAddress(pb, cvReadOnly); r != 0 {
		cfRelease(sbuf)
		return nil, fmt.Errorf("avfoundation: CVPixelBufferLockBaseAddress: %d", r)
	}
	base := cvPixelBufferGetBaseAddress(pb)
	w := int(cvPixelBufferGetWidth(pb))
	h2 := int(cvPixelBufferGetHeight(pb))
	stride := int(cvPixelBufferGetBytesPerRow(pb))
	if base == nil || w <= 0 || h2 <= 0 || stride <= 0 {
		cvPixelBufferUnlockBaseAddress(pb, cvReadOnly)
		cfRelease(sbuf)
		return nil, fmt.Errorf("avfoundation: empty pixel buffer %dx%d stride %d", w, h2, stride)
	}
	f := &Frame{
		Width:  w,
		Height: h2,
		Stride: stride,
		Format: format,
		PTS:    cmSampleBufferGetPTS(sbuf).duration(),
		Pix:    unsafe.Slice((*byte)(base), stride*h2),
	}
	// The pixel buffer belongs to the sample buffer, so the sample buffer must
	// outlive every read of Pix. Both are given up together, here.
	f.release = func() {
		cvPixelBufferUnlockBaseAddress(pb, cvReadOnly)
		cfRelease(sbuf)
	}
	return f, nil
}

func darwinClose(h handle) error {
	dr, ok := h.(*darwinReader)
	if !ok || dr == nil {
		return nil
	}
	dr.reader.Send(objc.Sel("cancelReading"))
	dr.output.Send(objc.Sel("release"))
	dr.reader.Send(objc.Sel("release"))
	dr.output, dr.reader = 0, 0
	return nil
}
