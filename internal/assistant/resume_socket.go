package assistant

// Codex --listen unix:// speaks WebSocket over a Unix socket. The CLI's
// legacy app-server proxy forwards raw JSONL and is not compatible with it.
import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

type unixJSONSocket struct {
	net.Conn
	reader  *bufio.Reader
	pending []byte
}

func connectUnixJSON(ctx context.Context, path string) (*unixJSONSocket, error) {
	c, e := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if e != nil {
		return nil, errors.New("CLI 本机连接不可用")
	}
	deadline := time.Now().Add(8 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = c.SetDeadline(deadline)
	fail := func() (*unixJSONSocket, error) {
		c.Close()
		return nil, errors.New("CLI 本机 WebSocket 握手失败")
	}
	var nonce [16]byte
	if _, e = rand.Read(nonce[:]); e != nil {
		return fail()
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])
	if _, e = fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", key); e != nil {
		return fail()
	}
	reader := bufio.NewReaderSize(c, 4096)
	response, e := http.ReadResponse(reader, &http.Request{Method: "GET"})
	if e != nil {
		return fail()
	}
	hash := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	if response.StatusCode != 101 || !strings.EqualFold(response.Header.Get("Upgrade"), "websocket") || response.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(hash[:]) || response.Header.Get("Sec-WebSocket-Extensions") != "" {
		return fail()
	}
	return &unixJSONSocket{Conn: c, reader: reader}, nil
}
func (c *unixJSONSocket) frame(op byte, data []byte) error {
	if len(data) > 4<<20 {
		return errors.New("CLI 消息超出上限")
	}
	frame := []byte{0x80 | op}
	switch {
	case len(data) < 126:
		frame = append(frame, 0x80|byte(len(data)))
	case len(data) <= 65535:
		frame = append(frame, 0x80|126, byte(len(data)>>8), byte(len(data)))
	default:
		frame = append(frame, 0x80|127)
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(data)))
		frame = append(frame, size[:]...)
	}
	var mask [4]byte
	if _, e := rand.Read(mask[:]); e != nil {
		return e
	}
	frame = append(frame, mask[:]...)
	for i, b := range data {
		frame = append(frame, b^mask[i%4])
	}
	for len(frame) > 0 {
		n, e := c.Conn.Write(frame)
		if e != nil {
			return e
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		frame = frame[n:]
	}
	return nil
}
func (c *unixJSONSocket) Write(b []byte) (int, error) {
	if e := c.frame(1, bytes.TrimSuffix(b, []byte{'\n'})); e != nil {
		return 0, e
	}
	return len(b), nil
}
func (c *unixJSONSocket) Read(out []byte) (int, error) {
	if len(c.pending) == 0 {
		var message []byte
		started := false
		for frames := 0; frames < 4096; frames++ {
			var h [2]byte
			if _, e := io.ReadFull(c.reader, h[:]); e != nil {
				return 0, e
			}
			final, op := h[0]&0x80 != 0, h[0]&15
			if h[0]&0x70 != 0 || h[1]&0x80 != 0 {
				return 0, errors.New("CLI WebSocket 帧无效")
			}
			n := uint64(h[1] & 127)
			if n == 126 {
				var b [2]byte
				if _, e := io.ReadFull(c.reader, b[:]); e != nil {
					return 0, e
				}
				n = uint64(binary.BigEndian.Uint16(b[:]))
			} else if n == 127 {
				var b [8]byte
				if _, e := io.ReadFull(c.reader, b[:]); e != nil {
					return 0, e
				}
				n = binary.BigEndian.Uint64(b[:])
			}
			if n > 32<<20 || len(message)+int(n) > 32<<20 {
				return 0, errors.New("CLI WebSocket 响应超出上限")
			}
			b := make([]byte, int(n))
			if _, e := io.ReadFull(c.reader, b); e != nil {
				return 0, e
			}
			if op >= 8 {
				if !final || n > 125 {
					return 0, errors.New("CLI 控制帧无效")
				}
				if op == 8 {
					return 0, io.EOF
				}
				if op == 9 {
					if e := c.frame(10, b); e != nil {
						return 0, e
					}
				} else if op != 10 {
					return 0, errors.New("CLI 控制帧不支持")
				}
				continue
			}
			if op == 1 && !started {
				started = true
			} else if op != 0 || !started {
				return 0, errors.New("CLI WebSocket 消息类型无效")
			}
			message = append(message, b...)
			if final {
				c.pending = append(message, '\n')
				break
			}
		}
		if len(c.pending) == 0 {
			return 0, errors.New("CLI WebSocket 分片过多")
		}
	}
	n := copy(out, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}
