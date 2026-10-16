package session

import "net/netip"

func isPrivate(host string) bool {
	a, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return a.IsPrivate() || a.IsLoopback()
}
