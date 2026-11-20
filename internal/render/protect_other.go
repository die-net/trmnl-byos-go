//go:build !linux

package render

// ProtectSelf does nothing outside Linux, where the shim refuses to start
// (see CheckDedicatedUser).
func ProtectSelf() error { return nil }
