//go:build linux

package helper

import (
	"errors"
	"net"
	"syscall"
)

// SOPeerCred — учётные данные клиента через SO_PEERCRED. Значения
// фиксируются ядром в момент connect() и не подделываются клиентом.
func SOPeerCred(conn *net.UnixConn) (Cred, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return Cred{}, err
	}
	var cred *syscall.Ucred
	var serr error
	if err := raw.Control(func(fd uintptr) {
		cred, serr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return Cred{}, err
	}
	if serr != nil {
		return Cred{}, serr
	}
	if cred == nil {
		return Cred{}, errors.New("SO_PEERCRED: нет данных")
	}
	return Cred{PID: int(cred.Pid), UID: int(cred.Uid), GID: int(cred.Gid)}, nil
}
