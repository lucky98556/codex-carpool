package quota

import (
	"net/netip"
	"strings"
)

// AllowsIP uses the client address supplied by the trusted ingress, never a
// client-controlled forwarding chain. Missing or malformed IPs fail closed.
func (policy KeyPolicy) AllowsIP(raw string) bool {
	if !policy.IPWhitelistEnabled {
		return true
	}
	address, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil || address.Zone() != "" {
		return false
	}
	address = address.Unmap()
	for _, entry := range policy.IPWhitelist {
		if prefix, err := netip.ParsePrefix(entry); err == nil {
			if prefix.Contains(address) {
				return true
			}
		} else if allowed, err := netip.ParseAddr(entry); err == nil && allowed.Unmap() == address {
			return true
		}
	}
	return false
}
