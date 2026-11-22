//go:build linux

package helper

import (
	"os/exec"
	"syscall"
)

// setProcessGroup запускает команду в собственной группе процессов и при
// отмене убивает всю группу, чтобы не оставались дочерние процессы.
// При остановке самого helper дочерние процессы завершает systemd
// (KillMode=control-group); Pdeathsig не используется — он привязан к
// потоку ОС, а не к процессу Go.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
