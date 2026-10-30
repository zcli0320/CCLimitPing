//go:build !darwin && !linux

package assistant

import (
	"errors"
	"net"
)

func runtimeSocketPeer(conn *net.UnixConn) (int, int, error) {
	return 0, 0, errors.New("当前系统不支持运行核验连接")
}
