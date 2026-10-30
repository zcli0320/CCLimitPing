//go:build linux

package assistant

import (
	"errors"
	"golang.org/x/sys/unix"
	"net"
)

func runtimeSocketPeer(conn *net.UnixConn) (int, int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var pid, uid int
	var inner error
	err = raw.Control(func(fd uintptr) {
		cred, e := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if e != nil {
			inner = e
			return
		}
		pid, uid = int(cred.Pid), int(cred.Uid)
	})
	if err != nil || inner != nil || pid <= 0 {
		return 0, 0, errors.New("无法确认本机连接进程")
	}
	return pid, uid, nil
}
