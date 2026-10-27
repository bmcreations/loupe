//go:build !darwin

package awake

// System does nothing off macOS, where loupe has no sleep to prevent.
func System() (release func(), err error) { return func() {}, nil }
