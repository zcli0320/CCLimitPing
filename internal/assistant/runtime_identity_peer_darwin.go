//go:build darwin

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
		cred, e := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if e != nil {
			inner = e
			return
		}
		uid = int(cred.Uid)
		pid, inner = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	})
	if err != nil || inner != nil || pid <= 0 {
		return 0, 0, errors.New("无法确认本机连接进程")
	}
	return pid, uid, nil
}
