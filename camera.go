// Copyright (c) the go-macos authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package avfoundation

import "fmt"

// Camera is a video capture device this machine can see.
//
// It is what [Cameras] returns, and its [Camera.ID] is what identifies one
// afterwards. The id is AVFoundation's unique device identifier, which is
// stable across unplugging and replugging the same device into the same port —
// unlike the position in the list, which moves whenever anything else is
// attached.
type Camera struct {
	// ID is AVFoundation's uniqueID. Pass it to open the device.
	ID string
	// Name is what the device calls itself, for a person to read. It is often
	// NOT distinctive: two different headsets can both report "USB Camera",
	// because the name comes from the camera module rather than the product it
	// was built into.
	Name string
	// Model is the manufacturer's model identifier, which on USB carries the
	// vendor and product ids — "UVC Camera VendorID_3141 ProductID_25448". It
	// is what tells two identically-named devices apart.
	Model string
	// Formats are the capture shapes the device offers.
	Formats []CameraFormat
}

// CameraFormat is one shape a camera can deliver.
type CameraFormat struct {
	// Width and Height are the picture's dimensions.
	Width, Height int
	// MinFPS and MaxFPS bound the frame rates this format supports. A device
	// that offers one rate reports it as both.
	MinFPS, MaxFPS float64
	// PixelFormat is what the device delivers, as the same four-character code
	// [PixelFormat] already carries for decoded files.
	//
	// ⚠ It is worth reading rather than assuming. A USB camera commonly offers
	// only packed 4:2:2 and no planar format at all: asking such a device for
	// a format it does not have does not fail loudly, it simply never
	// delivers — which looks exactly like a camera that is not working. That
	// is measured, not imagined: ffmpeg asked a headset's camera for yuv420p
	// and HUNG rather than refusing.
	PixelFormat PixelFormat
}

// String renders a format the way a person would say it.
func (f CameraFormat) String() string {
	if f.MinFPS == f.MaxFPS {
		return fmt.Sprintf("%dx%d %s @%gfps", f.Width, f.Height, f.PixelFormat, f.MaxFPS)
	}
	return fmt.Sprintf("%dx%d %s @%g-%gfps", f.Width, f.Height, f.PixelFormat, f.MinFPS, f.MaxFPS)
}

// String names the camera the way an error message should.
func (c Camera) String() string {
	if c.Model == "" {
		return fmt.Sprintf("%s (%s)", c.Name, c.ID)
	}
	return fmt.Sprintf("%s [%s]", c.Name, c.Model)
}

// Best returns the largest format the camera offers, preferring the highest
// frame rate among formats of that size, or false when it offers none.
//
// Largest by PIXEL COUNT rather than by width: a camera that offers 1280x1024
// alongside 1920x1080 should not have the wider-but-smaller one chosen because
// its first number is bigger.
func (c Camera) Best() (CameraFormat, bool) {
	var best CameraFormat
	var found bool
	for _, f := range c.Formats {
		switch {
		case !found,
			f.Width*f.Height > best.Width*best.Height,
			f.Width*f.Height == best.Width*best.Height && f.MaxFPS > best.MaxFPS:
			best, found = f, true
		}
	}
	return best, found
}
