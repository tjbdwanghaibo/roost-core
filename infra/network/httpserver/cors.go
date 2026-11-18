package httpserver

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// CORS 按 Origin 白名单放行浏览器的跨域调用：允许的 Origin 在响应上拿到 Access-Control-Allow-Origin，
// 预检（OPTIONS）答 204；不在白名单里的拿不到这个头，预检答 403。
//
// 只给凭证走请求体或 Authorization 头、不走 cookie 的接口用（登录入口 portal、大厅）：它从不发
// Access-Control-Allow-Credentials，浏览器不会带 cookie，所以放开 Origin 不会让别的站点借用浏览器里的登录态——
// 别的站点能发请求，但它手里没有请求体里那张凭证。白名单的作用是不让任意网页把这个入口当成自己的后端用。
//
// 它不拦请求本身：不允许的 Origin 的请求照常处理，只是浏览器读不到响应；非浏览器客户端本来就不受 CORS 约束。
// 要限流、要拒绝来源，放在入口前的反向代理上。
type CORS struct {
	origins []string
	any     bool
	methods string
	headers string
}

// NewCORS 构造一份白名单。origins 里的 "*" 表示任何 Origin；methods、headers 是预检里答复允许的方法与请求头。
// 调用方先用 CheckCORSOrigins 检查配置。
func NewCORS(origins, methods, headers []string) *CORS {
	return &CORS{
		origins: slices.Clone(origins),
		any:     slices.Contains(origins, "*"),
		methods: strings.Join(methods, ", "),
		headers: strings.Join(headers, ", "),
	}
}

// Allows 报告 origin 是否在白名单里（逐字节相同；空的、"null" 的 Origin 只有 "*" 放行，"null" 连 "*" 也不放）。
func (c *CORS) Allows(origin string) bool {
	if c == nil || origin == "" || origin == "null" {
		return false
	}
	return c.any || slices.Contains(c.origins, origin)
}

// Handle 给 next 的响应加 CORS 头：允许的 Origin 回写在 Access-Control-Allow-Origin 上；总是加 Vary: Origin
// （响应随 Origin 不同，缓存不能混用）。
func (c *CORS) Handle(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c.allowOrigin(w, r)
		next(w, r)
	}
}

// Preflight 答浏览器的预检请求（OPTIONS）：允许的 Origin 答 204 与允许的方法、请求头（缓存 10 分钟）；否则 403。
func (c *CORS) Preflight(w http.ResponseWriter, r *http.Request) {
	if !c.allowOrigin(w, r) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	w.Header().Set("Access-Control-Allow-Methods", c.methods)
	w.Header().Set("Access-Control-Allow-Headers", c.headers)
	w.Header().Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
}

func (c *CORS) allowOrigin(w http.ResponseWriter, r *http.Request) bool {
	AddVary(w.Header(), "Origin")
	origin := r.Header.Get("Origin")
	if !c.Allows(origin) {
		return false
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	return true
}

// CheckCORSOrigins 检查配置里的 Origin 白名单（key 是配置键名，用来点名）：每一项是 "*"，或浏览器发的 Origin 的
// 样子——http / https 的 scheme://host[:port]，不带路径（连结尾的 / 也不带）、查询、片段与用户信息，主机名小写。写成
// https://play.example.com/ 这种多一个斜杠的，浏览器发的 Origin 永远对不上，所以在读配置时就拒绝。
//
// 契约：通过检查的每一项都与浏览器（WHATWG URL 序列化）发出的 Origin 逐字节相同。同一个地址有多种写法时只收浏览器的
// 那一种，别的写法拒绝并给出该写的值，而不是悄悄改写——配置里写的就是比较时用的：
//   - 端口：默认端口（https 的 443、http 的 80）浏览器省掉（RV-20261118-08）；前导零浏览器去掉（:0443 → 省略、
//     :08443 → :8443，RV-20261118-10）；空端口（"…:"）浏览器省掉；只收 1～65535。
//   - 数字主机（BrowserHost）：IPv4 只收点分十进制（127.1、010.0.0.1 这类浏览器会改写），IPv6 只收浏览器的压缩写法；
//     浏览器解析不了的（192.0.2.300）拒绝。
func CheckCORSOrigins(key string, origins []string) error {
	for _, origin := range origins {
		if origin == "*" {
			continue
		}
		// 重新拼出的 scheme://host 必须与原文逐字节相同：带路径、查询、片段、用户信息、大写 scheme 的写法都对不上。
		parsed, err := url.Parse(origin)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
			parsed.Host != strings.ToLower(parsed.Host) || origin != parsed.Scheme+"://"+parsed.Host {
			return fmt.Errorf("config: %s entry %q is not an origin like https://example.com or https://example.com:8443 (no path, no trailing slash)", key, origin)
		}
		host, err := BrowserHost(parsed)
		if err != nil {
			return fmt.Errorf("config: %s entry %q: %v", key, origin, err)
		}
		// 按浏览器的序列化重新拼一遍，与原文比较。
		serialized := parsed.Scheme + "://" + host
		defaultPort := false
		if port := parsed.Port(); port != "" {
			number, err := strconv.Atoi(port)
			if err != nil || number < 1 || number > 65535 {
				return fmt.Errorf("config: %s entry %q has port %q; it must be 1..65535", key, origin, port)
			}
			defaultPort = (parsed.Scheme == "https" && number == 443) || (parsed.Scheme == "http" && number == 80)
			if !defaultPort {
				serialized += ":" + strconv.Itoa(number)
			}
		}
		if serialized == origin {
			continue
		}
		if defaultPort {
			return fmt.Errorf("config: %s entry %q names the default port, which browsers leave out of Origin; write %q", key, origin, serialized)
		}
		return fmt.Errorf("config: %s entry %q is not how browsers write this origin in the Origin header; write %q", key, origin, serialized)
	}
	return nil
}
