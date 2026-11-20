//go:build !linux && !darwin

package render

func readOnlyFS(int) (bool, error) { return false, errNotLinux }
