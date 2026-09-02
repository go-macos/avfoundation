//go:build !darwin

package avfoundation

// On every non-darwin platform the player seam answers [ErrUnsupported] rather
// than being nil, so a consumer that cross-compiles gets a clean error from the
// same API instead of a nil-func panic.
func init() {
	newPlayerBackend = func(string, Options) (playerBackend, Info, error) {
		return nil, Info{}, ErrUnsupported
	}
}
