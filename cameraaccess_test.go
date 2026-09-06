// Copyright (c) 2026, the go-macos authors. All rights reserved.
// Use of this source code is governed by a BSD-3-Clause license that can be
// found in the LICENSE file.

package avfoundation

import (
	"strings"
	"testing"
)

// ⚠ NOT-DETERMINED IS NOT GRANTED. It is the state where the camera WILL work,
// after a prompt somebody has to answer -- so a report that called it granted
// would describe a state that does not exist yet, and a person would go looking
// for a different reason their camera did nothing.
func TestOnlyAnAnsweredYesCountsAsGranted(t *testing.T) {
	if !CameraAuthorized.Granted() {
		t.Error("an authorized camera is not reported as granted")
	}
	for _, a := range []CameraAccess{CameraNotDetermined, CameraRestricted, CameraDenied, CameraAccess(99)} {
		if a.Granted() {
			t.Errorf("%v is reported as granted", a)
		}
	}
}

// Each state says something a person can act on, or says plainly that they
// cannot: "forbidden by a policy" is the one where Privacy & Security is the
// wrong advice.
func TestEveryStateSaysWhatItIs(t *testing.T) {
	for a, want := range map[CameraAccess]string{
		CameraNotDetermined: "not asked",
		CameraRestricted:    "policy",
		CameraDenied:        "refused",
		CameraAuthorized:    "granted",
		CameraAccess(99):    "unknown",
	} {
		if got := a.String(); !strings.Contains(got, want) {
			t.Errorf("CameraAccess(%d) says %q, want something containing %q", int(a), got, want)
		}
	}
}

// The values are AVAuthorizationStatus's own, and they are not ours to choose:
// they cross the boundary as raw integers from Objective-C.
func TestTheValuesAreTheOnesAVFoundationSends(t *testing.T) {
	for a, want := range map[CameraAccess]int{
		CameraNotDetermined: 0, CameraRestricted: 1, CameraDenied: 2, CameraAuthorized: 3,
	} {
		if int(a) != want {
			t.Errorf("%v is %d, and AVAuthorizationStatus says %d", a, int(a), want)
		}
	}
}

// ⭐ ASKING MUST NOT PROMPT. There is no way to assert the absence of a dialog
// from inside the process, so what this holds is the next best thing: asking
// repeatedly is cheap, answers the same thing every time, and returns a state
// this package knows. A call that had started a session would be neither.
func TestAskingIsCheapAndRepeatable(t *testing.T) {
	first := CameraAuthorization()
	for range 20 {
		if got := CameraAuthorization(); got != first {
			t.Fatalf("the answer changed from %v to %v without anything being asked", first, got)
		}
	}
	switch first {
	case CameraNotDetermined, CameraRestricted, CameraDenied, CameraAuthorized:
		t.Logf("this TEST BINARY's camera state is %v — a decision belongs to a CODE IDENTITY, so it says nothing about any other program", first)
	default:
		t.Errorf("this Mac answered %d, which this package does not name", int(first))
	}
}
