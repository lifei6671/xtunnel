package route

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/idna"
)

var (
	// ErrInvalidHost 表示公网请求的 Host 无法规范化为唯一、安全的路由键。
	ErrInvalidHost = errors.New("invalid HTTP host")
	// ErrInvalidPath 表示请求路径在 Router 与 Origin 之间无法保证一致解释。
	// HTTP Ingress 应把该错误映射为 400 INVALID_PATH，不得尝试清理后继续转发。
	ErrInvalidPath = errors.New("invalid HTTP path")
)

// HTTPMatch 是一次成功匹配得到的不可变结果。
//
// Path 与 RawPath 来自已经和 RequestURI 交叉校验的同一个 URL。M4-03 必须继续转发
// 原请求及这组路径语义，不得重新 parse、path.Clean 或改写重复斜线和尾斜线。
type HTTPMatch struct {
	Route    HTTPRoute
	Hostname string
	Path     string
	RawPath  string
}

// MatchHTTP 对一个公网 HTTP 请求执行精确 Host + 最长路径段前缀匹配。
//
// 匹配只读取已发布的内存 Snapshot，不查询 SQLite。方法先完成 Host 规范化和路径歧义
// 检查，再进入路由选择；因此非法编码不会因为“恰好没有路由”而绕过 400 拒绝语义。
func (snapshot *Snapshot) MatchHTTP(request *http.Request) (HTTPMatch, bool, error) {
	pathValue, rawPath, err := validatedRequestPath(request)
	if err != nil {
		return HTTPMatch{}, false, err
	}

	hostname, err := canonicalRequestHostname(request.Host)
	if err != nil {
		return HTTPMatch{}, false, err
	}
	if snapshot == nil {
		return HTTPMatch{}, false, nil
	}

	hostRoutes, exists := snapshot.http[hostname]
	if !exists {
		return HTTPMatch{}, false, nil
	}

	// Host 内的 Route 数量预期很小，直接扫描能保持 Snapshot 构建顺序稳定，同时明确
	// 选择最长前缀。相同 canonical prefix 已在完整快照构建时被拒绝，不存在平局歧义。
	var matched HTTPRoute
	found := false
	for index := range hostRoutes.routes {
		candidate := hostRoutes.routes[index]
		if !pathPrefixMatches(candidate.PathPrefix, pathValue) {
			continue
		}
		if !found || len(candidate.PathPrefix) > len(matched.PathPrefix) {
			matched = candidate
			found = true
		}
	}
	if !found {
		return HTTPMatch{}, false, nil
	}

	return HTTPMatch{
		Route:    matched,
		Hostname: hostname,
		Path:     pathValue,
		RawPath:  rawPath,
	}, true, nil
}

// CanonicalHostname 把 Route 写入值规范化为 Snapshot 的唯一持久化查找键。
// 它接受可选端口、带方括号的 IPv6 或 canonical bare IP；公网请求必须改用内部的
// canonicalRequestHostname，以额外强制 HTTP authority 的 IPv6 方括号规则。
func CanonicalHostname(hostport string) (string, error) {
	return canonicalHostname(hostport, false)
}

// canonicalRequestHostname 只用于公网请求 Host；它在做同一套 IDNA 规范化前，
// 额外强制 HTTP authority 语法：
// IPv6 必须使用方括号，方括号不得用于 IPv4。持久化键则继续使用无方括号 canonical IP。
func canonicalRequestHostname(authority string) (string, error) {
	return canonicalHostname(authority, true)
}

func canonicalHostname(hostport string, strictAuthority bool) (string, error) {
	if hostport == "" || !utf8.ValidString(hostport) || containsHostForbiddenByte(hostport) {
		return "", ErrInvalidHost
	}
	if address := net.ParseIP(hostport); address != nil {
		if strictAuthority && strings.Contains(hostport, ":") {
			return "", ErrInvalidHost
		}
		// 允许 canonical 的无方括号 IPv6 作为持久化键；带端口的 IPv6 Host 仍必须
		// 使用方括号，避免把地址末段误判成端口。
		return strings.ToLower(address.String()), nil
	}

	host, err := splitHostPort(hostport)
	if err != nil {
		return "", err
	}
	host, ok := trimSingleTrailingIDNADot(host)
	if !ok {
		return "", ErrInvalidHost
	}

	if address := net.ParseIP(host); address != nil {
		return strings.ToLower(address.String()), nil
	}
	if strings.Contains(host, ":") {
		// 非方括号 IPv6 与域名中的冒号会让 Host/Port 边界产生两种解释。
		return "", ErrInvalidHost
	}

	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil {
		return "", ErrInvalidHost
	}
	ascii = strings.ToLower(ascii)
	if !validDNSHostname(ascii) {
		return "", ErrInvalidHost
	}
	return ascii, nil
}

// trimSingleTrailingIDNADot 在 IDNA 映射前处理 ASCII 点和 UTS #46 的三种等价句点。
// 只移除一个根标签分隔符；连续尾点仍是畸形域名，不能被静默折叠。
func trimSingleTrailingIDNADot(host string) (string, bool) {
	last, size := utf8.DecodeLastRuneInString(host)
	if !isIDNADot(last) {
		return host, host != ""
	}
	host = host[:len(host)-size]
	if host == "" {
		return "", false
	}
	previous, _ := utf8.DecodeLastRuneInString(host)
	return host, !isIDNADot(previous)
}

func isIDNADot(character rune) bool {
	return character == '.' || character == '\u3002' || character == '\uff0e' || character == '\uff61'
}

// CanonicalPathPrefix 规范化 HTTP Route 的写入值。
// 根路径固定为 "/"；非根路径仅移除尾部斜线，不折叠重复斜线或其他路径内容。
func CanonicalPathPrefix(pathPrefix string) (string, error) {
	if pathPrefix == "" || pathPrefix[0] != '/' || strings.ContainsAny(pathPrefix, "?#") {
		return "", ErrInvalidPath
	}
	decoded, err := validateEscapedPath(pathPrefix)
	if err != nil {
		return "", err
	}
	if decoded != "/" {
		decoded = strings.TrimRight(decoded, "/")
		if decoded == "" {
			decoded = "/"
		}
	}
	if !validCanonicalPathPrefix(decoded) {
		return "", ErrInvalidPath
	}
	return decoded, nil
}

// validatedRequestPath 将 Go 已解析的 URL 与原始 RequestURI 逐项对照。
// Context 取消不会影响这个纯内存步骤；任一表达不一致都快速失败，避免 Router 按一种
// 路径命中而 ReverseProxy 再按另一种路径发送给 Origin。
func validatedRequestPath(request *http.Request) (string, string, error) {
	if request == nil || request.URL == nil || request.RequestURI == "" || request.RequestURI[0] != '/' {
		return "", "", ErrInvalidPath
	}
	for _, character := range request.RequestURI {
		if unicode.IsControl(character) || character == '#' {
			// Fragment 不属于 HTTP request-target；明文 # 即使位于 RawQuery，也可能在
			// URL 重序列化时改变 query/fragment 边界，因此统一 fail closed。
			return "", "", ErrInvalidPath
		}
	}
	if request.URL.Scheme != "" || request.URL.Host != "" || request.URL.User != nil || request.URL.Opaque != "" ||
		request.URL.Fragment != "" || request.URL.RawFragment != "" {
		return "", "", ErrInvalidPath
	}

	parsed, err := url.ParseRequestURI(request.RequestURI)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" || parsed.Opaque != "" || parsed.Fragment != "" {
		return "", "", ErrInvalidPath
	}
	targetPath := request.RequestURI
	if separator := strings.IndexByte(targetPath, '?'); separator >= 0 {
		targetPath = targetPath[:separator]
	}
	if targetPath == "" || parsed.Path != request.URL.Path || parsed.RawQuery != request.URL.RawQuery ||
		parsed.ForceQuery != request.URL.ForceQuery || request.URL.EscapedPath() != targetPath {
		return "", "", ErrInvalidPath
	}

	if request.URL.RawPath != "" {
		decoded, decodeErr := url.PathUnescape(request.URL.RawPath)
		if decodeErr != nil || decoded != request.URL.Path || request.URL.RawPath != targetPath {
			return "", "", ErrInvalidPath
		}
	}

	decoded, err := validateEscapedPath(targetPath)
	if err != nil || decoded != request.URL.Path {
		return "", "", ErrInvalidPath
	}
	return request.URL.Path, request.URL.RawPath, nil
}

// validateEscapedPath 在保留 URL Path 原始形态的前提下拒绝会跨解析器改变边界的输入。
// encoded separator 会改变段结构；dot segment 可能被 Origin 或中间件消解；控制字符和
// 反斜杠则在不同 HTTP 实现中存在歧义。这里明确拒绝，而不是调用 path.Clean 猜测意图。
func validateEscapedPath(escaped string) (string, error) {
	if escaped == "" || escaped[0] != '/' || !utf8.ValidString(escaped) {
		return "", ErrInvalidPath
	}
	return validatePathEncodingLayers(escaped, true)
}

// validatePathEncodingLayers 检查一次 URL 解码以及有限次下游重复解码。
// strictFirst=true 用于公网 escaped path，首层非法 escape 必须失败；false 用于已经
// 解码并持久化的 canonical prefix，普通 literal percent 可以保留，但任何可再次解释为
// separator、dot segment 或控制字符的序列仍会使 Snapshot 构建失败。
func validatePathEncodingLayers(value string, strictFirst bool) (string, error) {
	// 某些 Origin 框架会在进入应用前再次解码路径。逐层检查可以拒绝 `%252F`、
	// `%255C`、`%252E%252E` 等二次解释后改变段边界的输入。层数设为有限上限；
	// 超过上限仍可继续解码的输入直接失败，避免攻击者用嵌套编码制造无界 CPU 工作。
	const maxDecodeDepth = 8
	current := value
	firstDecoded := ""
	for depth := 0; depth < maxDecodeDepth; depth++ {
		decoded, changed, err := decodePathLayer(current, depth == 0 && strictFirst)
		if err != nil {
			return "", err
		}
		if depth == 0 {
			if strictFirst {
				firstDecoded = decoded
			} else {
				firstDecoded = value
			}
		}
		if !utf8.ValidString(decoded) || containsPathForbiddenByte(decoded) {
			return "", ErrInvalidPath
		}

		for _, segment := range strings.Split(decoded, "/") {
			if segment == "." || segment == ".." {
				return "", ErrInvalidPath
			}
		}
		if !changed || !strings.Contains(decoded, "%") {
			return firstDecoded, nil
		}
		current = decoded
	}
	return "", ErrInvalidPath
}

// decodePathLayer 只解码语法完整的 %HH，并在更深层把其他 percent 当作普通字符继续
// 扫描。这样 `%ZZ` 不会让其后的 `%2F` 逃过检查；首层公网输入则仍严格拒绝任意非法 escape。
func decodePathLayer(value string, strict bool) (string, bool, error) {
	var decoded strings.Builder
	decoded.Grow(len(value))
	changed := false
	for index := 0; index < len(value); {
		if value[index] != '%' {
			decoded.WriteByte(value[index])
			index++
			continue
		}
		if index+2 >= len(value) {
			if strict {
				return "", false, ErrInvalidPath
			}
			decoded.WriteByte('%')
			index++
			continue
		}
		decodedByte, ok := decodeHexByte(value[index+1], value[index+2])
		if !ok {
			if strict {
				return "", false, ErrInvalidPath
			}
			decoded.WriteByte('%')
			index++
			continue
		}
		if decodedByte == '/' || decodedByte == '\\' || decodedByte < 0x20 || decodedByte == 0x7f {
			return "", false, ErrInvalidPath
		}
		decoded.WriteByte(decodedByte)
		changed = true
		index += 3
	}
	return decoded.String(), changed, nil
}

// validCanonicalPathPrefix 校验已经解码、准备进入 SQLite/Snapshot 的唯一形式。
// 与公网请求校验分开，是因为编码后的合法保留字符会以解码值持久化；构建阶段既验证
// 该值本身，也拒绝再次解释会改变路径段边界的残留编码，并确认尾斜线规则已经执行。
func validCanonicalPathPrefix(pathPrefix string) bool {
	if pathPrefix == "" || pathPrefix[0] != '/' || !utf8.ValidString(pathPrefix) || containsPathForbiddenByte(pathPrefix) {
		return false
	}
	if pathPrefix != "/" && strings.HasSuffix(pathPrefix, "/") {
		return false
	}
	for _, segment := range strings.Split(pathPrefix, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	_, err := validatePathEncodingLayers(pathPrefix, false)
	return err == nil
}

func pathPrefixMatches(prefix, requestPath string) bool {
	if prefix == "/" {
		return strings.HasPrefix(requestPath, "/")
	}
	return requestPath == prefix || strings.HasPrefix(requestPath, prefix+"/")
}

func splitHostPort(hostport string) (string, error) {
	if strings.HasPrefix(hostport, "[") {
		closing := strings.LastIndexByte(hostport, ']')
		if closing < 0 {
			return "", ErrInvalidHost
		}
		host := hostport[1:closing]
		remainder := hostport[closing+1:]
		if remainder != "" {
			if remainder[0] != ':' || !validPort(remainder[1:]) {
				return "", ErrInvalidHost
			}
		}
		address := net.ParseIP(host)
		if address == nil || address.To4() != nil {
			return "", ErrInvalidHost
		}
		return host, nil
	}

	colonCount := strings.Count(hostport, ":")
	if colonCount == 0 {
		return hostport, nil
	}
	if colonCount != 1 {
		return "", ErrInvalidHost
	}
	separator := strings.LastIndexByte(hostport, ':')
	if separator <= 0 || !validPort(hostport[separator+1:]) {
		return "", ErrInvalidHost
	}
	return hostport[:separator], nil
}

func validPort(port string) bool {
	if port == "" {
		return false
	}
	for index := range len(port) {
		if port[index] < '0' || port[index] > '9' {
			return false
		}
	}
	_, err := strconv.ParseUint(port, 10, 16)
	return err == nil
}

func validDNSHostname(host string) bool {
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for index := range len(label) {
			character := label[index]
			if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
				return false
			}
		}
	}
	return true
}

func containsHostForbiddenByte(value string) bool {
	for index := range len(value) {
		character := value[index]
		if character <= 0x20 || character == 0x7f || strings.ContainsRune("/@\\?#%", rune(character)) {
			return true
		}
	}
	return false
}

func containsPathForbiddenByte(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) || character == '\\' {
			return true
		}
	}
	return false
}

func decodeHexByte(high, low byte) (byte, bool) {
	highValue, highOK := hexValue(high)
	lowValue, lowOK := hexValue(low)
	return highValue<<4 | lowValue, highOK && lowOK
}

func hexValue(value byte) (byte, bool) {
	switch {
	case value >= '0' && value <= '9':
		return value - '0', true
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10, true
	case value >= 'A' && value <= 'F':
		return value - 'A' + 10, true
	default:
		return 0, false
	}
}
