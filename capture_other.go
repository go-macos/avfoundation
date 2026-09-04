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
