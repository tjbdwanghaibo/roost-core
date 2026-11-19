package httpserver

import (
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// BrowserHost 按浏览器的规则（WHATWG URL 标准的主机解析）解析 u 的主机，返回浏览器序列化后的写法；浏览器解析不了时返回错误。
// 给“写在配置里、交给浏览器用”的地址做检查：跨域白名单的 Origin（CheckCORSOrigins，浏览器发来的 Origin 要逐字节相同）、
// 给 WebGL / TS 客户端的 WebSocket 地址（service/account 的 CheckWebSocketAddr，浏览器要能建连）。
//
// 只处理数字主机，DNS 名原样返回（大小写、国际化域名不在这里管）：
//
//   - 不在方括号里的主机不能含冒号（不带方括号的 IPv6，如 ws://2001:db8::1:7080/ws，浏览器解析不了）：拒绝并提示写成 [...]。
//   - 方括号里的是 IPv6：必须是合法的 IPv6（不能是 IPv4、不带 %区域号），返回浏览器的压缩写法（小写、最长一段连续零压成
//     ::、IPv4 映射地址也写成十六进制，如 [::ffff:c000:21e]）。
//   - 末段是数字（十进制，或 0x 开头的十六进制；末尾的一个点先去掉）的主机名，浏览器一律按 IPv4 解析：每段可以是十进制、
//     0 开头的八进制、0x 开头的十六进制，至多 4 段，最后一段可以占满剩下的字节（127.1 即 127.0.0.1）。解析失败
//     （192.0.2.300、09.1.1.1、game.1）就是浏览器打不开的地址；成功时返回点分十进制。
//
// 调用方比较返回值与原写法：不同就是浏览器会改写的写法（:0443 那类端口问题由调用方自己处理）。
func BrowserHost(u *url.URL) (string, error) {
	host := u.Hostname()
	if strings.HasPrefix(u.Host, "[") {
		addr, err := netip.ParseAddr(host)
		if err != nil || !addr.Is6() || addr.Zone() != "" {
			return "", fmt.Errorf("host [%s] is not an IPv6 address browsers accept (no IPv4 in brackets, no %%zone)", host)
		}
		return "[" + whatwgIPv6(addr) + "]", nil
	}
	// 不在方括号里的主机，浏览器读到第一个冒号就当端口开始：ws://2001:db8::1:7080/ws 解析失败。Go 的 url.Parse 只对
	// http / https 拒绝这种写法（GODEBUG urlstrictcolons），ws / wss 照样把最后一个冒号之前当主机，所以在这里拒绝。
	if strings.Contains(host, ":") {
		return "", fmt.Errorf("host %q has a colon outside brackets, which browsers cannot parse; write an IPv6 address in brackets, like [2001:db8::1]:7080", host)
	}
	ipv4, numeric, err := whatwgIPv4(host)
	if err != nil {
		return "", fmt.Errorf("host %q ends in a number, so browsers read it as an IPv4 address, and it is not one: %v", host, err)
	}
	if numeric {
		return ipv4, nil
	}
	return host, nil
}

// whatwgIPv4 是 WHATWG 的“末段是数字”判断加 IPv4 解析。numeric 为假时主机是 DNS 名，不按 IPv4 处理。
func whatwgIPv4(host string) (addr string, numeric bool, err error) {
	parts := strings.Split(strings.ToLower(host), ".")
	if parts[len(parts)-1] == "" {
		if len(parts) == 1 {
			return "", false, nil
		}
		parts = parts[:len(parts)-1]
	}
	last := parts[len(parts)-1]
	if last == "" || (!allDigits(last) && !(strings.HasPrefix(last, "0x") && isHex(last[2:]))) {
		return "", false, nil
	}
	if len(parts) > 4 {
		return "", true, fmt.Errorf("%d parts; at most 4", len(parts))
	}
	numbers := make([]uint64, len(parts))
	for i, part := range parts {
		n, ok := ipv4Number(part)
		if !ok {
			return "", true, fmt.Errorf("part %q is not a decimal, 0-led octal or 0x hex number", part)
		}
		if i < len(parts)-1 && n > 255 {
			return "", true, fmt.Errorf("part %q is over 255", part)
		}
		numbers[i] = n
	}
	value := numbers[len(numbers)-1]
	if limit := uint64(1) << (8 * (5 - len(parts))); value >= limit {
		return "", true, fmt.Errorf("the last part %q is %d or more", parts[len(parts)-1], limit)
	}
	for i, n := range numbers[:len(numbers)-1] {
		value += n << (8 * (3 - i))
	}
	return netip.AddrFrom4([4]byte{byte(value >> 24), byte(value >> 16), byte(value >> 8), byte(value)}).String(), true, nil
}

// ipv4Number 解析 WHATWG 的一段 IPv4 数字：0x 开头十六进制（0x 本身是 0）、0 开头八进制、否则十进制。
func ipv4Number(part string) (uint64, bool) {
	if part == "" {
		return 0, false
	}
	base := 10
	switch {
	case len(part) >= 2 && part[:2] == "0x":
		part, base = part[2:], 16
	case len(part) >= 2 && part[0] == '0':
		part, base = part[1:], 8
	}
	if part == "" {
		return 0, true
	}
	n, err := strconv.ParseUint(part, base, 64)
	return n, err == nil
}

// whatwgIPv6 按 WHATWG 的 IPv6 序列化：八段十六进制小写、不带前导零，最长（并列取最前）的一段长度至少 2 的连续零段压成 ::。
// 与 netip 的 String 只差在 IPv4 映射地址：WHATWG 不写点分形式。
func whatwgIPv6(addr netip.Addr) string {
	raw := addr.As16()
	var pieces [8]uint16
	for i := range pieces {
		pieces[i] = uint16(raw[2*i])<<8 | uint16(raw[2*i+1])
	}
	start, length := -1, 1
	for i := 0; i < 8; {
		if pieces[i] != 0 {
			i++
			continue
		}
		j := i
		for j < 8 && pieces[j] == 0 {
			j++
		}
		if j-i > length {
			start, length = i, j-i
		}
		i = j
	}
	var b strings.Builder
	for i := 0; i < 8; i++ {
		if i == start {
			b.WriteString("::")
			i += length - 1
			continue
		}
		if b.Len() > 0 && !strings.HasSuffix(b.String(), ":") {
			b.WriteByte(':')
		}
		b.WriteString(strconv.FormatUint(uint64(pieces[i]), 16))
	}
	return b.String()
}

func allDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

func isHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
