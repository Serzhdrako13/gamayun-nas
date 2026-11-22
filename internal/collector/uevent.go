package collector

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// RelevantUevent — kernel uevent блочного устройства (add/remove/change).
// Формат: "ACTION@DEVPATH\0KEY=VALUE\0...". Сообщения udev (libudev,
// группа 2) сюда не попадают — слушаем только группу ядра.
func RelevantUevent(msg []byte) bool {
	var subsys, action bool
	for _, f := range bytes.Split(msg, []byte{0}) {
		k, v, ok := bytes.Cut(f, []byte{'='})
		if !ok {
			continue
		}
		switch string(k) {
		case "SUBSYSTEM":
			subsys = string(v) == "block"
		case "ACTION":
			action = string(v) == "add" || string(v) == "remove" || string(v) == "change"
		}
	}
	return subsys && action
}

// ListenUevents слушает kernel uevents до отмены ctx и вызывает fire на
// каждое блочное событие и при переполнении буфера (события потеряны —
// нужна сверка). Ошибка — сокет не открыт (например, AF_NETLINK запрещён
// юнитом): коллектор работает только по таймеру.
func ListenUevents(ctx context.Context, fire func()) error {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.NETLINK_KOBJECT_UEVENT)
	if err != nil {
		return fmt.Errorf("netlink uevent: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: 1}); err != nil {
		unix.Close(fd)
		return fmt.Errorf("netlink uevent bind: %w", err)
	}
	// Recvfrom блокируется; закрытие fd при отмене ctx его прерывает не
	// всегда, поэтому таймаут чтения 1 с и проверка ctx в цикле.
	tv := unix.Timeval{Sec: 1}
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		unix.Close(fd)
		return fmt.Errorf("netlink uevent: %w", err)
	}
	go func() {
		defer unix.Close(fd)
		buf := make([]byte, 64<<10)
		for ctx.Err() == nil {
			n, from, err := unix.Recvfrom(fd, buf, 0)
			switch {
			case errors.Is(err, unix.EAGAIN), errors.Is(err, unix.EINTR):
				continue
			case errors.Is(err, unix.ENOBUFS):
				fire()
				continue
			case err != nil:
				fire() // сокет сломан — последняя сверка; дальше только таймер
				return
			}
			if sa, ok := from.(*unix.SockaddrNetlink); !ok || sa.Pid != 0 {
				continue // не от ядра
			}
			if RelevantUevent(buf[:n]) {
				fire()
			}
		}
	}()
	return nil
}
