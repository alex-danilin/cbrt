// Package hostcheck holds the Host-header checks that keep a loopback-only
// HTTP server from answering a web page that reached it through a
// DNS-rebinding domain. The gateway (internal/server) and the projects page
// (internal/projects) share them.
package hostcheck

import (
	"net"
	"strings"
)

// Name strips the port and IPv6 brackets from a Host header value.
func Name(h string) string {
	if hh, _, err := net.SplitHostPort(h); err == nil {
		return hh
	}
	return strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
}

// Loopback reports whether h (a host name without port) is localhost or a
// loopback IP literal.
func Loopback(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// LoopbackHeader reports whether a Host header value names a loopback host.
func LoopbackHeader(hostHeader string) bool { return Loopback(Name(hostHeader)) }
