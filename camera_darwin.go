// Copyright (c) the go-macos authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build darwin

package avfoundation

import (
	"fmt"

	"github.com/go-macos/objc"
)

// Cameras lists the video capture devices this machine can see.
//
// It asks AVFoundation rather than the USB bus, so what comes back is what can
// actually be opened: a headset's camera appears here as an ordinary device,
// and so does the Mac's own.
//
// ⚠ It does NOT turn anything on, and it does not ask for permission. A camera
// is only powered when a session runs, which is also when its indicator light
// comes on -- and macOS asks the person then, not now. Listing is safe to do at
// start-up; opening is not something to do without being asked.
//
// Devices with no usable format are left out rather than returned empty: a
// caller choosing from this list should not have to check whether each entry
// can do anything.
func Cameras() ([]Camera, error) {
	if err := load(); err != nil {
		return nil, err
	}
	var out []Camera
	var failure error
	objc.AutoreleasePool(func() {
		// AVCaptureDeviceDiscoverySession rather than the deprecated
		// +devicesWithMediaType:, which macOS 26 still answers but which
		// reports nothing for some external cameras.
		types := objc.ClassID("NSArray").Send(objc.Sel("arrayWithObjects:count:"),
			&[]objc.ID{objc.NSString("AVCaptureDeviceTypeExternal"),
				objc.NSString("AVCaptureDeviceTypeBuiltInWideAngleCamera")}[0], uint64(2))

		session := objc.ClassID("AVCaptureDeviceDiscoverySession").Send(
			objc.Sel("discoverySessionWithDeviceTypes:mediaType:position:"),
			types, objc.NSString("vide"), uint64(0)) // 0 = AVCaptureDevicePositionUnspecified
		if session == 0 {
			failure = fmt.Errorf("avfoundation: no capture discovery session")
			return
		}
		devices := objc.Send[objc.ID](session, objc.Sel("devices"))
		n := objc.Send[uint64](devices, objc.Sel("count"))
		for i := uint64(0); i < n; i++ {
			dev := objc.Send[objc.ID](devices, objc.Sel("objectAtIndex:"), i)
			c := Camera{
				ID:    objc.GoString(objc.Send[objc.ID](dev, objc.Sel("uniqueID"))),
				Name:  objc.GoString(objc.Send[objc.ID](dev, objc.Sel("localizedName"))),
				Model: objc.GoString(objc.Send[objc.ID](dev, objc.Sel("modelID"))),
			}
			c.Formats = formatsOf(dev)
			if len(c.Formats) == 0 {
				continue
			}
			out = append(out, c)
		}
	})
	return out, failure
}

// formatsOf reads one device's capture shapes.
//
// The dimensions come from the format description rather than from any
// convenience property: -[AVCaptureDeviceFormat formatDescription] is the
// authority, and CMVideoFormatDescriptionGetDimensions is what reads it.
func formatsOf(dev objc.ID) []CameraFormat {
	formats := objc.Send[objc.ID](dev, objc.Sel("formats"))
	n := objc.Send[uint64](formats, objc.Sel("count"))
	out := make([]CameraFormat, 0, n)
	for i := uint64(0); i < n; i++ {
		f := objc.Send[objc.ID](formats, objc.Sel("objectAtIndex:"), i)
		desc := objc.Send[objc.ID](f, objc.Sel("formatDescription"))
		if desc == 0 {
			continue
		}
		dim := cmVideoDimensions(desc)
		if dim.width <= 0 || dim.height <= 0 {
			continue
		}
		cf := CameraFormat{
			Width:       int(dim.width),
			Height:      int(dim.height),
			PixelFormat: PixelFormat(cmMediaSubType(desc)),
		}
		cf.MinFPS, cf.MaxFPS = rateRange(f)
		out = append(out, cf)
	}
	return out
}

// rateRange reads the widest frame-rate range a format offers.
//
// A format carries a LIST of ranges, and a device that offers 5, 10, 15, 20,
// 25 and 30 frames a second reports six of them rather than one 5-to-30. What
// a caller wants to know is what is reachable, so this takes the extremes
// across the whole list.
func rateRange(format objc.ID) (minFPS, maxFPS float64) {
	ranges := objc.Send[objc.ID](format, objc.Sel("videoSupportedFrameRateRanges"))
	n := objc.Send[uint64](ranges, objc.Sel("count"))
	for i := uint64(0); i < n; i++ {
		r := objc.Send[objc.ID](ranges, objc.Sel("objectAtIndex:"), i)
		lo := objc.Send[float64](r, objc.Sel("minFrameRate"))
		hi := objc.Send[float64](r, objc.Sel("maxFrameRate"))
		if i == 0 {
			minFPS, maxFPS = lo, hi
			continue
		}
		if lo < minFPS {
			minFPS = lo
		}
		if hi > maxFPS {
			maxFPS = hi
		}
	}
	return minFPS, maxFPS
}
