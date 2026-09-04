// Copyright (c) the go-macos authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package avfoundation

import (
	"errors"
	"strings"
	"testing"
)

// TestNamingACameraThatIsNotThereIsAnError, and not a fallback.
//
// ⛔ A caller who named a camera meant THAT camera. Quietly opening a different
// one is the failure that cannot be seen from the code: somebody asks for the
// view out of a headset and gets a picture of their own face, and every line
// says it worked.
func TestNamingACameraThatIsNotThereIsAnError(t *testing.T) {
	all := []Camera{
		{ID: "aaa", Name: "FaceTime HD Camera"},
		{ID: "bbb", Name: "USB Camera"},
	}
	got, err := pickCamera(all, "ccc")
	if !errors.Is(err, ErrNoCamera) {
		t.Fatalf("pickCamera = %v, %v; want ErrNoCamera", got, err)
	}
	// And the error says what IS there, because "no such camera" on a machine
	// with two of them is a sentence that ends a search rather than starting
	// one.
	for _, want := range []string{"FaceTime HD Camera", "USB Camera"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

// TestNoCameraAtAll.
func TestNoCameraAtAll(t *testing.T) {
	if _, err := pickCamera(nil, ""); !errors.Is(err, ErrNoCamera) {
		t.Errorf("an empty machine gave %v", err)
	}
	if _, err := pickCamera(nil, "aaa"); !errors.Is(err, ErrNoCamera) {
		t.Errorf("an empty machine gave %v", err)
	}
}

// TestNamingNothingTakesTheFirst, which is what a caller with one camera means.
func TestNamingNothingTakesTheFirst(t *testing.T) {
	all := []Camera{{ID: "aaa", Name: "first"}, {ID: "bbb", Name: "second"}}
	got, err := pickCamera(all, "")
	if err != nil || got.ID != "aaa" {
		t.Errorf("pickCamera = %v, %v; want the first", got, err)
	}
	if got, err := pickCamera(all, "bbb"); err != nil || got.ID != "bbb" {
		t.Errorf("pickCamera = %v, %v; want the second", got, err)
	}
}

// TestWhatEachAnswerFromTCCMeans.
//
// ⛔ NOT-DETERMINED IS NOT A REFUSAL. A bundled program that starts a session
// before anyone has been asked makes macOS put its own prompt up, which is the
// right moment for it -- the person is told what wants the camera at the
// instant something wants it. Refusing here would mean this package raised the
// prompt itself, which needs an Objective-C block.
func TestWhatEachAnswerFromTCCMeans(t *testing.T) {
	if err := errorFor(authAuthorized); err != nil {
		t.Errorf("an authorised camera was refused: %v", err)
	}
	if err := errorFor(authNotDetermined); err != nil {
		t.Errorf("a camera nobody has been asked about was refused: %v", err)
	}
	for _, a := range []authorization{authDenied, authRestricted, authorization(99)} {
		err := errorFor(a)
		if !errors.Is(err, ErrCameraDenied) {
			t.Errorf("status %d gave %v, want ErrCameraDenied", int(a), err)
		}
	}
	// Restricted says WHY it is different, because "denied" on a managed Mac
	// sends a person to a setting they cannot change.
	if got := errorFor(authRestricted).Error(); !strings.Contains(got, "policy") {
		t.Errorf("restricted reads as %q", got)
	}
	// And an answer this package does not know reports the number rather than
	// pretending it is one of the four: a new macOS may add one.
	if got := errorFor(authorization(99)).Error(); !strings.Contains(got, "99") {
		t.Errorf("an unknown status reads as %q", got)
	}
}

// TestALogIsOptional: every entry point takes a Logf and nil must be silence
// rather than a crash on the first line.
func TestALogIsOptional(t *testing.T) {
	captureLog(nil)("this goes nowhere, and does not panic: %d", 1)
	said := ""
	captureLog(func(f string, a ...any) { said = f })("something")
	if said != "something" {
		t.Errorf("the log said %q", said)
	}
}

// TestTheBiggestFormatIsByAREA, not by width.
//
// A camera offering 1280x1024 beside 1920x1080 must not have the wider but
// smaller one chosen because its first number is bigger.
func TestTheBiggestFormatIsByArea(t *testing.T) {
	c := Camera{Formats: []CameraFormat{
		{Width: 1920, Height: 1080, MaxFPS: 30},
		{Width: 1280, Height: 1024, MaxFPS: 60},
		{Width: 640, Height: 480, MaxFPS: 120},
	}}
	best, ok := c.Best()
	if !ok || best.Width != 1920 {
		t.Errorf("Best = %v, %v; want the 1920x1080", best, ok)
	}
	// Between two of the same size, the faster one.
	c = Camera{Formats: []CameraFormat{
		{Width: 1920, Height: 1080, MaxFPS: 30},
		{Width: 1920, Height: 1080, MaxFPS: 60},
	}}
	if best, _ := c.Best(); best.MaxFPS != 60 {
		t.Errorf("Best = %v; want the 60fps one", best)
	}
	// A camera with no usable format says so rather than handing back a zero.
	if _, ok := (Camera{}).Best(); ok {
		t.Error("a camera with no formats reported one")
	}
}

// TestHowAFormatReads, because it goes in front of a person choosing one.
func TestHowAFormatReads(t *testing.T) {
	one := CameraFormat{Width: 1920, Height: 1080, MinFPS: 30, MaxFPS: 30, PixelFormat: BGRA}
	if got := one.String(); got != "1920x1080 BGRA @30fps" {
		t.Errorf("a single-rate format reads as %q", got)
	}
	many := CameraFormat{Width: 640, Height: 480, MinFPS: 5, MaxFPS: 30, PixelFormat: BGRA}
	if got := many.String(); got != "640x480 BGRA @5-30fps" {
		t.Errorf("a ranged format reads as %q", got)
	}
}

// TestHowACameraReads: the MODEL is what tells two identically-named devices
// apart, and two USB cameras really are both called "USB Camera".
func TestHowACameraReads(t *testing.T) {
	with := Camera{ID: "x", Name: "USB Camera", Model: "UVC Camera VendorID_3141 ProductID_25448"}
	if got := with.String(); !strings.Contains(got, "VendorID_3141") {
		t.Errorf("a camera with a model reads as %q", got)
	}
	without := Camera{ID: "0x1234", Name: "FaceTime HD Camera"}
	if got := without.String(); !strings.Contains(got, "0x1234") {
		t.Errorf("a camera with no model reads as %q; it should fall back to the id", got)
	}
}
