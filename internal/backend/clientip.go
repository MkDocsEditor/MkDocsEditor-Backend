package backend

import (
	"fmt"
	"net"
	"strings"

	"github.com/labstack/echo/v4"
)

// ParseTrustedProxies parses server.trustedProxies (CIDRs or single addresses)
func ParseTrustedProxies(entries []string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if !strings.Contains(e, "/") {
			ip := net.ParseIP(e)
			if ip == nil {
				return out, fmt.Errorf("%q is not an IP address or CIDR", e)
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		_, n, err := net.ParseCIDR(e)
		if err != nil {
			return out, fmt.Errorf("%q is not an IP address or CIDR", e)
		}
		out = append(out, n)
	}
	return out, nil
}

// ipExtractor returns the client address: the TCP peer, or with trusted proxies the
// right-most address of X-Forwarded-For that is not a trusted proxy
func (rs *RestService) ipExtractor() echo.IPExtractor {
	if len(rs.trustedProxies) == 0 {
		return echo.ExtractIPDirect()
	}
	opts := []echo.TrustOption{echo.TrustLoopback(false), echo.TrustLinkLocal(false), echo.TrustPrivateNet(false)}
	for _, n := range rs.trustedProxies {
		opts = append(opts, echo.TrustIPRange(n))
	}
	return echo.ExtractIPFromXFFHeader(opts...)
}

// securityHeaders adds the headers of docs/auth.md section 11 that middleware.Secure does not set
func (rs *RestService) securityHeaders(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		h := c.Response().Header()
		h.Set("Cross-Origin-Resource-Policy", "same-site")
		if rs.isHTTPS(c) {
			h.Set(echo.HeaderStrictTransportSecurity, "max-age=31536000")
		}
		p := c.Request().URL.Path
		if strings.HasPrefix(p, "/auth/") || strings.HasPrefix(p, "/account/") || strings.HasPrefix(p, "/admin/") {
			h.Set(echo.HeaderCacheControl, "no-store")
		}
		return next(c)
	}
}
