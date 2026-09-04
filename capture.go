// Copyright (c) the go-macos authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package avfoundation

import (
	"errors"
	"fmt"
)

// Errors a camera can refuse with, which are worth telling apart: two of them
// are a person's decision and one is a build problem.
var (
	// ErrNoCamera is returned when there is no video device to open, or when
	// the one named by [CaptureOptions.Camera] is not attached.
	ErrNoCamera = errors.New("avfoundation: no such camera")

	// ErrCameraDenied means the person has said no, in System Settings or at
	// the prompt. Nothing a program does changes that: the only way back is
	// Privacy & Security > Camera.
	ErrCameraDenied = errors.New("avfoundation: the camera was refused by this Mac's owner")

	// ErrNoUsageDescription means this program has no NSCameraUsageDescription
	// in its Info.plist.
	//
	// ⛔ IT IS NOT A PERMISSION PROBLEM, IT IS A BUILD PROBLEM, and it is the
	// one that must be caught here: macOS does not deny a program that asks for
	// the camera without a usage description, it KILLS it -- the process is
	// terminated by TCC with "This app has crashed because it attempted to
	// access privacy-sensitive data without a usage description". A library
	// that let that happen would take its caller down with no error to catch
	// and nothing in a log.
	//
	// The fix is to ship a bundle: see go-macos/appbundle, and give it an
	// NSCameraUsageDescription saying what the camera is for. A bare binary run
	// from a terminal has no Info.plist at all.
	ErrNoUsageDescription = errors.New("avfoundation: this program has no NSCameraUsageDescription; " +
		"a camera cannot be opened from a bare binary, only from an app bundle")
)

// CaptureOptions are the choices a caller makes about a camera.
type CaptureOptions struct {
	// Camera is the [Camera.ID] to open. Empty opens the first one [Cameras]
	// lists, which is the Mac's own on a machine with nothing else attached.
	Camera string

	// Logf receives progress. A nil Logf says nothing.
	Logf func(string, ...any)
}

// Capture is a running camera.
//
// ⚠ THE LIGHT IS ON while it exists. On every Mac with a camera indicator the
// hardware wires the light to the sensor's power, so it cannot be lit without
// the camera running and cannot be dark while it is. Close when done, and mean
// it: a Capture left open is a camera left on.
//
// It is safe to use from several goroutines.
type Capture struct{ *capture }

// Latest is the newest frame the camera has delivered, or false before the
// first one arrives.
//
// ⛔ NEWEST, NOT NEXT. Frames are dropped rather than queued: a camera runs at
// its own rate and a caller reads at its own, and the two are never the same.
// A queue between them either grows without bound or blocks the delivery
// callback, and a blocked callback is a camera that stops. What a viewer wants
// on screen is what the camera sees now, so an unread frame is worth nothing
// and is thrown away.
//
// The Frame owns its own memory: it is copied out of the capture buffer before
// the callback returns, so there is nothing to release and holding one costs
// only what it is.
func (c Capture) Latest() (*Frame, bool) { return c.latest() }

// Camera is the device this is running, as [Cameras] described it.
func (c Capture) Camera() Camera { return c.device }

// Close stops the camera and turns its light off. It is safe to call twice.
func (c Capture) Close() error { return c.close() }

// captureLog is the logging seam every capture shares, so a nil Logf costs one
// branch at open rather than one at every frame.
func captureLog(fn func(string, ...any)) func(string, ...any) {
	if fn == nil {
		return func(string, ...any) {}
	}
	return fn
}

// pickCamera chooses the device to open from what is attached.
//
// An explicit id that is not there is an ERROR and not a fallback: a caller who
// named a camera meant that one, and quietly opening a different one would put
// the wrong picture in front of somebody who asked for a headset's view and got
// their own face.
func pickCamera(all []Camera, want string) (Camera, error) {
	if len(all) == 0 {
		return Camera{}, fmt.Errorf("%w: nothing is attached", ErrNoCamera)
	}
	if want == "" {
		return all[0], nil
	}
	for _, c := range all {
		if c.ID == want {
			return c, nil
		}
	}
	return Camera{}, fmt.Errorf("%w: %q is not attached; there is %s",
		ErrNoCamera, want, names(all))
}

// names lists what IS attached, for the error that says one is not.
func names(all []Camera) string {
	out := ""
	for i, c := range all {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("%q", c.Name)
	}
	return out
}

// authorization is what this Mac has decided about the camera, in the order
// AVAuthorizationStatus uses.
type authorization int

const (
	authNotDetermined authorization = 0
	authRestricted    authorization = 1
	authDenied        authorization = 2
	authAuthorized    authorization = 3
)

// errorFor turns a status into the refusal a caller should see, or nil when
// there is nothing to refuse.
//
// Restricted is reported as denied on purpose: it is a Mac under a policy the
// person cannot change, and telling them to visit Privacy & Security is the
// same advice with one more sentence they cannot act on.
func errorFor(a authorization) error {
	switch a {
	case authAuthorized:
		return nil
	case authNotDetermined:
		// Nobody has been asked. Starting the session is what asks them, and
		// macOS puts its own prompt up -- so this is not a refusal.
		return nil
	case authDenied:
		return ErrCameraDenied
	case authRestricted:
		return fmt.Errorf("%w: this Mac is under a policy that forbids it", ErrCameraDenied)
	default:
		return fmt.Errorf("%w: this Mac answered %d, which this package does not know",
			ErrCameraDenied, int(a))
	}
}
