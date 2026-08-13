package ws

import (
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
)

// clientIP returns a canonical, unzoned IP for rate-limit identity. Forwarding
// headers are considered only when the TCP peer itself is an explicitly trusted
// proxy. The chain is walked from the nearest hop to the origin, selecting the
// first untrusted hop so client-supplied prefixes cannot override the address
// appended by a correctly configured proxy.
func (s *Server) clientIP(r *http.Request) string {
	direct, ok := parseRemoteIP(r.RemoteAddr)
	if !ok {
		// An invalid transport address cannot safely opt into proxy handling.
		return r.RemoteAddr
	}
	if !s.isTrustedProxy(direct) {
		return direct.String()
	}

	var (
		chain []netip.Addr
		valid bool
	)
	if values := r.Header.Values("Forwarded"); len(values) > 0 {
		chain, valid = parseForwarded(values)
	} else if values := r.Header.Values("X-Forwarded-For"); len(values) > 0 {
		chain, valid = parseXForwardedFor(values)
	}
	if !valid || len(chain) == 0 {
		return direct.String()
	}
	for i := len(chain) - 1; i >= 0; i-- {
		if !s.isTrustedProxy(chain[i]) {
			return chain[i].String()
		}
	}
	// Every advertised hop is trusted. The leftmost entry is still the
	// protocol-defined origin and is the least surprising stable identity.
	return chain[0].String()
}

func (s *Server) isTrustedProxy(addr netip.Addr) bool {
	addr = addr.Unmap()
	for _, prefix := range s.TrustedProxyPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func parseRemoteIP(remote string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	addr, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil || addr.Zone() != "" {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

func parseForwarded(values []string) ([]netip.Addr, bool) {
	elements, ok := splitOutsideQuotes(strings.Join(values, ","), ',')
	if !ok || len(elements) == 0 {
		return nil, false
	}
	chain := make([]netip.Addr, 0, len(elements))
	for _, element := range elements {
		params, ok := splitOutsideQuotes(element, ';')
		if !ok {
			return nil, false
		}
		found := false
		for _, param := range params {
			key, value, ok := strings.Cut(param, "=")
			if !ok || strings.TrimSpace(key) == "" {
				return nil, false
			}
			if !strings.EqualFold(strings.TrimSpace(key), "for") {
				continue
			}
			if found {
				return nil, false
			}
			text, ok := unquoteForwardedValue(strings.TrimSpace(value))
			if !ok {
				return nil, false
			}
			addr, ok := parseForwardedNode(text)
			if !ok {
				return nil, false
			}
			chain = append(chain, addr)
			found = true
		}
		if !found {
			return nil, false
		}
	}
	return chain, true
}

func parseXForwardedFor(values []string) ([]netip.Addr, bool) {
	parts := strings.Split(strings.Join(values, ","), ",")
	chain := make([]netip.Addr, 0, len(parts))
	for _, part := range parts {
		addr, ok := parseForwardedNode(strings.TrimSpace(part))
		if !ok {
			return nil, false
		}
		chain = append(chain, addr)
	}
	return chain, len(chain) > 0
}

func parseForwardedNode(text string) (netip.Addr, bool) {
	text = strings.TrimSpace(text)
	if text == "" || strings.EqualFold(text, "unknown") || strings.HasPrefix(text, "_") || strings.Contains(text, "%") {
		return netip.Addr{}, false
	}
	if addr, err := netip.ParseAddr(text); err == nil && addr.Zone() == "" {
		return addr.Unmap(), true
	}
	if strings.HasPrefix(text, "[") {
		end := strings.IndexByte(text, ']')
		if end < 0 {
			return netip.Addr{}, false
		}
		addr, err := netip.ParseAddr(text[1:end])
		if err != nil || addr.Zone() != "" || !validOptionalPort(text[end+1:]) {
			return netip.Addr{}, false
		}
		return addr.Unmap(), true
	}
	host, port, err := net.SplitHostPort(text)
	if err != nil || !validPort(port) {
		return netip.Addr{}, false
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || addr.Zone() != "" {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

func validOptionalPort(rest string) bool {
	return rest == "" || (strings.HasPrefix(rest, ":") && validPort(rest[1:]))
}

func validPort(port string) bool {
	n, err := strconv.ParseUint(port, 10, 16)
	return err == nil && n > 0
}

func unquoteForwardedValue(value string) (string, bool) {
	if !strings.HasPrefix(value, `"`) {
		return value, !strings.ContainsAny(value, `"\`)
	}
	if len(value) < 2 || value[len(value)-1] != '"' {
		return "", false
	}
	var out strings.Builder
	for i := 1; i < len(value)-1; i++ {
		c := value[i]
		if c == '\\' {
			i++
			if i >= len(value)-1 {
				return "", false
			}
			c = value[i]
		} else if c == '"' {
			return "", false
		}
		if c < 0x20 || c == 0x7f {
			return "", false
		}
		out.WriteByte(c)
	}
	return out.String(), true
}

func splitOutsideQuotes(value string, separator byte) ([]string, bool) {
	var (
		parts   []string
		start   int
		quoted  bool
		escaped bool
	)
	for i := 0; i < len(value); i++ {
		c := value[i]
		if escaped {
			escaped = false
			continue
		}
		if quoted && c == '\\' {
			escaped = true
			continue
		}
		if c == '"' {
			quoted = !quoted
			continue
		}
		if c == separator && !quoted {
			part := strings.TrimSpace(value[start:i])
			if part == "" {
				return nil, false
			}
			parts = append(parts, part)
			start = i + 1
		}
	}
	if quoted || escaped {
		return nil, false
	}
	part := strings.TrimSpace(value[start:])
	if part == "" {
		return nil, false
	}
	return append(parts, part), true
}
