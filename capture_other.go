// Copyright (c) the go-macos authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build !darwin

package avfoundation

// capture exists so that the portable half of this package compiles here. No
// value of it is ever made: there is no AVFoundation to make one from.
type capture struct{ device Camera }

// OpenCamera reports [ErrUnsupported]: a capture session is AVFoundation's, and
// AVFoundation is macOS's.
func OpenCamera(CaptureOptions) (*Capture, error) { return nil, ErrUnsupported }

// Cameras reports [ErrUnsupported] for the same reason.
func Cameras() ([]Camera, error) { return nil, ErrUnsupported }

func (c *capture) latest() (*Frame, bool) { return nil, false }
func (c *capture) close() error           { return ErrUnsupported }

// CameraAuthorization reports [CameraNotDetermined] away from macOS.
//
// Not "denied": nothing has refused anything. There is no camera to ask about
// and no decision to report, and calling that a refusal would send a caller
// looking for a setting to change.
func CameraAuthorization() CameraAccess { return CameraNotDetermined }
