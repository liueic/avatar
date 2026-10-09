package server

import (
	"net"
	"net/http"
)

// clientIPForLog renders the client address per the privacy policy of
// SPEC §11: by default the IP is omitted entirely; "trunc24" zeroes the last
// octet (v4) or the interface id (v6); "full" is opt-in only.
func clientIPForLog(r *http.Request, mode string) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	switch mode {
	case "full":
		return host
	case "trunc24":
		ip := net.ParseIP(host)
		if ip == nil {
			return ""
		}
		if v4 := ip.To4(); v4 != nil {
			return v4.Mask(net.CIDRMask(24, 32)).String()
		}
		return ip.Mask(net.CIDRMask(64, 128)).String()
	default: // "none" / ""
		return ""
	}
}
