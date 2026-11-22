//go:build !linux

package helper

import "os/exec"

func setProcessGroup(*exec.Cmd) {}
