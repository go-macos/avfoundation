// Copyright (c) the go-macos authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build darwin

package avfoundation

import (
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/go-macos/objc"
)

// capture is the running session behind [Capture].
type capture struct {
	device Camera
	logf   func(string, ...any)

	session objc.ID
	output  objc.ID
	target  objc.ID
	queue   uintptr

	mu     sync.Mutex
	frame  *Frame
	closed bool
}

// OpenCamera starts a camera and delivers its frames.
//
// ⚠ THE LIGHT COMES ON HERE. Listing cameras is free and silent; opening one is
// neither. On every Mac with an indicator the hardware ties the light to the
// sensor's power, so a running capture is a lit light and there is no way to
// have one without the other -- which is a feature and not a limitation.
//
// It refuses BEFORE touching the camera when this program has no
// NSCameraUsageDescription, because macOS does not deny such a program, it ends
// it. See [ErrNoUsageDescription].
func OpenCamera(o CaptureOptions) (*Capture, error) {
	if err := load(); err != nil {
		return nil, err
	}
	logf := captureLog(o.Logf)

	// ⛔ THE USAGE DESCRIPTION FIRST, before anything is asked of AVFoundation.
	// A program without one is terminated by TCC rather than refused, and a
	// library that let that happen would take its caller down with nothing to
	// catch and nothing in a log.
	if !hasCameraUsageDescription() {
		return nil, ErrNoUsageDescription
	}

	all, err := Cameras()
	if err != nil {
		return nil, err
	}
	device, err := pickCamera(all, o.Camera)
	if err != nil {
		return nil, err
	}

	// ⛔ NOT-DETERMINED IS ALLOWED THROUGH, and that is not a hole. A bundled
	// program that starts a capture session before anyone has been asked makes
	// macOS put the prompt up itself, which is the right moment for it: the
	// person is told what wants the camera at the instant something wants it.
	// Refusing here would mean this package had to raise the prompt on its own,
	// which needs an Objective-C BLOCK -- and a block is a struct with an
	// invoke pointer and a descriptor, not a function pointer, so it is real
	// machinery to build for a question the system already asks better.
	//
	// A refusal at the prompt then shows up as a session that runs and delivers
	// nothing, which is why Latest reports "not yet" rather than blocking.
	if err := errorFor(cameraAuthorization()); err != nil {
		return nil, err
	}

	c := &capture{device: device, logf: logf}
	if err := c.start(); err != nil {
		c.close()
		return nil, err
	}
	logf("the camera %q is running; its light is on until this is closed", device.Name)
	return &Capture{c}, nil
}

// start builds the session, the output and the delegate, and runs it.
func (c *capture) start() error {
	var oerr error
	objc.AutoreleasePool(func() {
		dev := objc.ClassID("AVCaptureDevice").Send(
			objc.Sel("deviceWithUniqueID:"), objc.NSString(c.device.ID))
		if dev == 0 {
			oerr = fmt.Errorf("%w: %q went away between listing and opening",
				ErrNoCamera, c.device.Name)
			return
		}
		input := objc.ClassID("AVCaptureDeviceInput").Send(
			objc.Sel("deviceInputWithDevice:error:"), dev, objc.ID(0))
		if input == 0 {
			oerr = fmt.Errorf("avfoundation: %q would not open as an input", c.device.Name)
			return
		}

		session := objc.ClassID("AVCaptureSession").Send(objc.Sel("alloc")).Send(objc.Sel("init"))
		if session == 0 {
			oerr = fmt.Errorf("avfoundation: no capture session")
			return
		}
		if !objc.Send[bool](session, objc.Sel("canAddInput:"), input) {
			session.Send(objc.Sel("release"))
			oerr = fmt.Errorf("avfoundation: the session would not take %q", c.device.Name)
			return
		}
		session.Send(objc.Sel("addInput:"), input)

		out := objc.ClassID("AVCaptureVideoDataOutput").Send(objc.Sel("alloc")).Send(objc.Sel("init"))

		// ⛔ BGRA IS ASKED OF THE OUTPUT, NOT OF THE DEVICE, and that is what
		// makes this work on every camera rather than on the ones that happen
		// to suit. A USB camera commonly offers only packed 4:2:2 and no packed
		// RGB at all -- asking the DEVICE for a format it does not have does
		// not fail loudly, it simply never delivers, which looks exactly like a
		// camera that is not working. Measured: ffmpeg asked a headset's camera
		// for yuv420p and HUNG rather than refusing. An output's videoSettings
		// are a CONVERSION request, which AVFoundation honours for any device.
		//
		// The key is built rather than Dlsym'd -- kCVPixelBufferPixelFormatType-
		// Key is the string "PixelFormatType" -- for the reason the media type
		// is: dereferencing the address of a global CFStringRef is the uintptr
		// conversion go vet's unsafeptr check rightly flags.
		settings := objc.ClassID("NSDictionary").Send(
			objc.Sel("dictionaryWithObject:forKey:"),
			objc.ClassID("NSNumber").Send(objc.Sel("numberWithUnsignedInt:"), uint32(BGRA)),
			objc.NSString("PixelFormatType"))
		out.Send(objc.Sel("setVideoSettings:"), settings)

		// ⛔ LATE FRAMES ARE DROPPED. Without this the output holds every frame
		// the delegate has not taken yet, and a delegate that falls one frame
		// behind falls further behind for ever -- the camera's rate does not
		// slow down to match. Dropping is also what makes Latest honest: what
		// arrives is what the camera sees NOW.
		out.Send(objc.Sel("setAlwaysDiscardsLateVideoFrames:"), true)

		target, err := c.delegate()
		if err != nil {
			session.Send(objc.Sel("release"))
			out.Send(objc.Sel("release"))
			oerr = err
			return
		}
		// A SERIAL queue of our own, which is what the delegate contract asks
		// for: frames must arrive one at a time, and the main queue is the one
		// place they must not arrive at -- a program drawing on the main thread
		// would be delivering frames to itself.
		c.queue = dispatchQueueCreate("com.go-macos.avfoundation.camera", 0)
		out.Send(objc.Sel("setSampleBufferDelegate:queue:"), target, c.queue)

		if !objc.Send[bool](session, objc.Sel("canAddOutput:"), out) {
			session.Send(objc.Sel("release"))
			out.Send(objc.Sel("release"))
			oerr = fmt.Errorf("avfoundation: the session would not take a video output")
			return
		}
		session.Send(objc.Sel("addOutput:"), out)

		c.session, c.output, c.target = session, out, target
		session.Send(objc.Sel("startRunning"))
	})
	return oerr
}

// delegate builds the object AVFoundation hands frames to.
//
// One CLASS for the process and one INSTANCE per capture: registering a class
// twice fails, and the class carries no state -- the instance is looked up in
// a table by its own pointer, so two cameras running at once cannot deliver
// into each other.
func (c *capture) delegate() (objc.ID, error) {
	if err := registerDelegateClass(); err != nil {
		return 0, err
	}
	obj := objc.ID(delegateClass).Send(objc.Sel("alloc")).Send(objc.Sel("init"))
	if obj == 0 {
		return 0, fmt.Errorf("avfoundation: the frame delegate would not be made")
	}
	liveMu.Lock()
	live[obj] = c
	liveMu.Unlock()
	return obj, nil
}

var (
	delegateOnce  sync.Once
	delegateClass objc.Class
	delegateErr   error

	liveMu sync.Mutex
	live   = map[objc.ID]*capture{}
)

// registerDelegateClass makes the Objective-C class once.
//
// ⛔ WITH THE PROTOCOL, not merely the method. AVCaptureVideoDataOutput checks
// conformsToProtocol: on what it is handed, and a class that implements
// captureOutput:didOutputSampleBuffer:fromConnection: without DECLARING the
// protocol is accepted by setSampleBufferDelegate:queue: and then never called
// -- a camera that runs, lights up, and delivers nothing.
func registerDelegateClass() error {
	delegateOnce.Do(func() {
		proto := objc.GetProtocol("AVCaptureVideoDataOutputSampleBufferDelegate")
		var protos []*objc.Protocol
		if proto != nil {
			protos = []*objc.Protocol{proto}
		}
		delegateClass, delegateErr = objc.RegisterClassWithProtocols(
			"GoMacOSCameraDelegate", objc.GetClass("NSObject"), protos,
			[]objc.MethodDef{{
				Cmd: objc.Sel("captureOutput:didOutputSampleBuffer:fromConnection:"),
				Fn:  gotFrame,
			}},
		)
	})
	return delegateErr
}

// gotFrame is called on the capture queue for every frame the camera delivers.
//
// ⛔ IT COPIES AND RETURNS. The sample buffer belongs to AVFoundation and is
// recycled the moment this returns -- keeping the pointer would hand a caller
// pixels that change under them, which does not crash and does not look like a
// mistake: it looks like a camera with a torn picture. So the frame is copied
// into Go memory here, on the capture queue, and what a caller holds is theirs.
func gotFrame(self objc.ID, _ objc.SEL, _ objc.ID, sample objc.ID, _ objc.ID) {
	liveMu.Lock()
	c := live[self]
	liveMu.Unlock()
	if c == nil {
		return
	}
	pb := cmSampleBufferGetImageBuffer(uintptr(sample))
	if pb == 0 {
		return
	}
	// 1 is kCVPixelBufferLock_ReadOnly: this never writes, and saying so lets
	// CoreVideo skip invalidating the GPU's copy.
	if cvPixelBufferLockBaseAddress(pb, 1) != 0 {
		return
	}
	defer cvPixelBufferUnlockBaseAddress(pb, 1)

	base := cvPixelBufferGetBaseAddress(pb)
	if base == nil {
		return
	}
	w := int(cvPixelBufferGetWidth(pb))
	h := int(cvPixelBufferGetHeight(pb))
	stride := int(cvPixelBufferGetBytesPerRow(pb))
	if w <= 0 || h <= 0 || stride < w*4 {
		return
	}
	pix := make([]byte, stride*h)
	copy(pix, unsafe.Slice((*byte)(base), stride*h))

	f := &Frame{
		Width: w, Height: h, Stride: stride, Format: BGRA,
		PTS: time.Duration(cmSampleBufferGetPTS(uintptr(sample)).duration()),
		Pix: pix,
	}
	c.mu.Lock()
	if !c.closed {
		c.frame = f
	}
	c.mu.Unlock()
}

// latest hands back the newest frame.
func (c *capture) latest() (*Frame, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.frame == nil {
		return nil, false
	}
	return c.frame, true
}

// close stops the session and lets the camera go.
func (c *capture) close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.frame = nil
	c.mu.Unlock()

	objc.AutoreleasePool(func() {
		if c.session != 0 {
			c.session.Send(objc.Sel("stopRunning"))
		}
		// The delegate is cleared BEFORE anything is released: a frame arriving
		// on the capture queue while this runs would otherwise reach a capture
		// whose session has gone.
		if c.output != 0 {
			c.output.Send(objc.Sel("setSampleBufferDelegate:queue:"), objc.ID(0), uintptr(0))
			c.output.Send(objc.Sel("release"))
			c.output = 0
		}
		if c.session != 0 {
			c.session.Send(objc.Sel("release"))
			c.session = 0
		}
	})
	if c.target != 0 {
		liveMu.Lock()
		delete(live, c.target)
		liveMu.Unlock()
		c.target.Send(objc.Sel("release"))
		c.target = 0
	}
	if c.queue != 0 {
		dispatchRelease(c.queue)
		c.queue = 0
	}
	c.logf("the camera is closed and its light is off")
	return nil
}

// hasCameraUsageDescription reports whether this program can lawfully ask.
//
// A bare binary has a main bundle with no Info.plist at all, so the answer is
// nil and this is false -- which is the whole point: see [ErrNoUsageDescription].
func hasCameraUsageDescription() bool {
	var ok bool
	objc.AutoreleasePool(func() {
		bundle := objc.ClassID("NSBundle").Send(objc.Sel("mainBundle"))
		if bundle == 0 {
			return
		}
		v := bundle.Send(objc.Sel("objectForInfoDictionaryKey:"),
			objc.NSString("NSCameraUsageDescription"))
		ok = v != 0 && objc.GoString(v) != ""
	})
	return ok
}

// cameraAuthorization is what this Mac has already decided.
func cameraAuthorization() authorization {
	return authorization(objc.Send[int64](
		objc.ClassID("AVCaptureDevice"),
		objc.Sel("authorizationStatusForMediaType:"), objc.NSString("vide")))
}
