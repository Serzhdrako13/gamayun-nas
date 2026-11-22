//go:build !linux

package apps

import "errors"

var errOS = errors.New("только Linux")

func chattrC(string) error          { return errOS }
func hasNoCOW(string) (bool, error) { return false, errOS }

// StatfsReadOnly — только Linux.
func StatfsReadOnly(string) (bool, error) { return false, errOS }
