// Copyright (c) the go-macos authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build darwin

package avfoundation

import (
	"errors"
	"testing"
)

// TestOnDeviceCamerasAnswer, and describe what they can do.
//
// ⚠ IT TURNS NOTHING ON. Listing is a question to AVFoundation about what
// exists; no session runs, no light comes on, and nobody is prompted. That is
// what makes it safe to do at start-up, and it is asserted here by the shape of
// what comes back rather than by trusting the sentence.
func TestOnDeviceCamerasAnswer(t *testing.T) {
	cams, err := Cameras()
	if err != nil {
		t.Fatalf("Cameras: %v", err)
	}
	if len(cams) == 0 {
		t.Skip("no camera attached to this Mac")
	}
	for _, c := range cams {
		t.Logf("%s", c)
		if c.ID == "" {
			t.Errorf("%q has no unique id, so nothing could open it later", c.Name)
		}
		if len(c.Formats) == 0 {
			t.Errorf("%s is listed with no format at all; such a device should "+
				"have been left out", c)
		}
		best, ok := c.Best()
		if !ok {
			t.Errorf("%s offers no best format", c)
			continue
		}
		t.Logf("    %d formats, best %s", len(c.Formats), best)
		if best.Width <= 0 || best.Height <= 0 {
			t.Errorf("%s: the best format is %dx%d", c, best.Width, best.Height)
		}
	}
}

// TestOnDeviceABareBinaryIsRefusedRatherThanKilled.
//
// ⛔ THE GUARD THAT MATTERS, and the only test that can see it. A go test
// binary has no Info.plist -- it is a bare Mach-O in a temporary directory --
// so this is exactly the situation TCC terminates a process for. If the check
// in OpenCamera were removed, this test would not FAIL: the whole test binary
// would be killed, and what a person would see is a suite that stops with no
// output and no failing test named.
//
// So the assertion is that it comes back at all, with the error that says what
// to do about it.
func TestOnDeviceABareBinaryIsRefusedRatherThanKilled(t *testing.T) {
	c, err := OpenCamera(CaptureOptions{Logf: t.Logf})
	if c != nil {
		_ = c.Close()
		t.Fatal("a bare test binary opened a camera; either this Mac has an " +
			"Info.plist where none was expected, or the guard is gone and the " +
			"next run of this suite will be killed rather than fail")
	}
	if !errors.Is(err, ErrNoUsageDescription) {
		// Not a failure on a machine with no camera at all: there is nothing to
		// refuse, and the guard is checked before the list on purpose.
		if errors.Is(err, ErrNoCamera) {
			t.Skipf("no camera on this Mac: %v", err)
		}
		t.Fatalf("OpenCamera = %v, want ErrNoUsageDescription", err)
	}
	t.Logf("refused, as it must be: %v", err)
}

// TestOnDeviceTheUsageDescriptionIsReadFromTheBundle.
//
// The check is a real question to NSBundle rather than a guess about the
// executable's path: a program run from a bundle answers, a bare binary does
// not, and this suite is the second kind.
func TestOnDeviceTheUsageDescriptionIsReadFromTheBundle(t *testing.T) {
	if hasCameraUsageDescription() {
		t.Error("a go test binary reports an NSCameraUsageDescription; it has " +
			"no Info.plist at all, so something is answering for a bundle that " +
			"is not this one")
	}
}

// TestOnDeviceTheAuthorizationStatusIsOneOfTheFour, whatever this Mac has
// decided: the number is AVAuthorizationStatus and a fifth value would mean a
// new macOS has added one.
func TestOnDeviceTheAuthorizationStatusIsOneOfTheFour(t *testing.T) {
	got := cameraAuthorization()
	switch got {
	case authNotDetermined, authRestricted, authDenied, authAuthorized:
		t.Logf("this Mac's camera authorization is %d", int(got))
	default:
		t.Errorf("authorizationStatusForMediaType: answered %d", int(got))
	}
}
