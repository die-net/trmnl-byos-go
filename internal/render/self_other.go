//go:build !linux

package render

import "errors"

type selfState struct{}

// snapshotSelf is not built here, so the shim refuses to start.
func snapshotSelf() (selfState, error) {
	return selfState{}, errors.New("checking the renderer's own state is built only for Linux")
}

func (selfState) changed(selfState) error { return nil }
