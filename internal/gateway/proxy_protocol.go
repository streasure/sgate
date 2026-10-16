package gateway

import (
	"bytes"
	"net/netip"
	"strings"
)

// parseProxyProtocolV1 解析 PROXY protocol v1 头（HAProxy 规范）。
// 格式：PROXY TCP4 <srcIP> <dstIP> <srcPort> <dstPort>\r\n
//
//	PROXY TCP6 <srcIP> <dstIP> <srcPort> <dstPort>\r\n
//	PROXY UNKNOWN\r\n（或省略后续字段）
//
// 返回：(真实客户端IP, 头总长度, ok)。ok=false 表示不是合法 PROXY 头
// （调用方应按普通首包处理）。UNKNOWN 变体返回空 IP + ok=true（头合法但
// 无法取 IP，连接保持代理 IP）。
func parseProxyProtocolV1(data []byte) (clientIP string, headerLen int, ok bool) {
	const prefix = "PROXY "
	if !bytes.HasPrefix(data, []byte(prefix)) {
		return "", 0, false
	}
	// 头以 \r\n 结尾，上限 107 字节（规范）
	idx := bytes.Index(data, []byte("\r\n"))
	if idx < 0 || idx > 107 {
		return "", 0, false
	}
	line := string(data[:idx])
	headerLen = idx + 2 // include \r\n

	fields := strings.Fields(line)
	// PROXY UNKNOWN\r\n → 2 fields; PROXY TCP4 ... → 6 fields
	if len(fields) == 2 && fields[1] == "UNKNOWN" {
		return "", headerLen, true
	}
	if len(fields) != 6 {
		return "", 0, false
	}
	if fields[0] != "PROXY" {
		return "", 0, false
	}
	proto := fields[1]
	if proto != "TCP4" && proto != "TCP6" {
		return "", 0, false
	}
	srcIP := fields[2]
	if _, err := netip.ParseAddr(srcIP); err != nil {
		return "", 0, false
	}
	// dstIP/srcPort/dstPort 合法性不强校验（只关心 srcIP）
	return srcIP, headerLen, true
}
