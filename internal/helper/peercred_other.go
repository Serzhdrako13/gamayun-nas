//go:build !linux

package helper

import (
	"errors"
	"net"
)

// SOPeerCred на других ОС не реализован: доступ всегда запрещён.
func SOPeerCred(*net.UnixConn) (Cred, error) {
	return Cred{}, errors.New("SO_PEERCRED поддерживается только в Linux")
}
