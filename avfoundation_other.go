//go:build !darwin

package avfoundation

// On every non-darwin platform the seams answer [ErrUnsupported] rather than
// being nil, so a consumer that cross-compiles for Linux or Windows gets a clean
// error from the same API instead of a nil-func panic.
func init() {
	openFile = func(string, PixelFormat) (handle, Info, error) { return nil, Info{}, ErrUnsupported }
	nextFrame = func(handle, PixelFormat) (*Frame, error) { return nil, ErrUnsupported }
	closeFile = func(handle) error { return nil }
}
