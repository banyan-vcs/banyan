// Package gitwire 实现 git 兼容层（docs/03-git-compat.md）：
// Smart HTTP v1 端点、upload-pack/receive-pack 会话、虚拟子树仓库路由、
// 服务端 filter。协议行为基准：git 源码 upload-pack.c / receive-pack.c /
// http-backend.c 与 Documentation/technical/ 下协议文档。
package gitwire

import (
	"bufio"
	"fmt"
	"io"
)

// pkt-line 编解码（gitprotocol-common 语义）：
//
//	<pkt-len> <payload>     pkt-len 含 4 字节长度本身，十六进制
//	0000                    flush-pkt
//	0001                    delim-pkt（v2 用，v1 备而不用）
//	0002                    response-end（v2）

const (
	pktFlush = 0
	pktDelim = 1
	maxPktLen = 65520
)

// WritePacket 写一条数据 pkt。
func WritePacket(w io.Writer, payload []byte) error {
	if len(payload) > maxPktLen-4 {
		return fmt.Errorf("pkt-line: payload too large (%d)", len(payload))
	}
	if _, err := fmt.Fprintf(w, "%04x", len(payload)+4); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// WriteFlush 写 flush-pkt。
func WriteFlush(w io.Writer) error {
	_, err := io.WriteString(w, "0000")
	return err
}

// ReadPacket 读一条 pkt。返回 (payload, isFlush, err)。
func ReadPacket(r *bufio.Reader) ([]byte, bool, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, false, err
	}
	var n int
	if _, err := fmt.Sscanf(string(lenBuf[:]), "%04x", &n); err != nil {
		return nil, false, fmt.Errorf("pkt-line: bad length %q", lenBuf)
	}
	if n == pktFlush {
		return nil, true, nil
	}
	if n < 4 {
		return nil, false, fmt.Errorf("pkt-line: invalid length %d", n)
	}
	payload := make([]byte, n-4)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, false, err
	}
	return payload, false, nil
}

// ReadPacketText 读一条文本 pkt（去尾换行）。
func ReadPacketText(r *bufio.Reader) (string, bool, error) {
	b, flush, err := ReadPacket(r)
	if err != nil || flush {
		return "", flush, err
	}
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return string(b), false, nil
}
