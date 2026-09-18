package hostagent

import (
	"net/netip"
)

// Structural egress denials. These apply to every CONNECT target regardless of
// the per-slot allowlist: a mis-issued allowlist entry must not be able to hand
// a runtime instance the cloud metadata service, a link-local neighbour or an
// out-of-band name resolution channel. There is deliberately no override flag.
//
// Scope boundary: this classifier only sees the literal CONNECT target. It
// cannot judge a hostname that the upstream proxy later resolves to a denied
// address, and it is not a kernel network gate. Host and peer-slot reachability
// still requires the Linux netns/firewall gate (VM0b) on a dedicated host.
type egressDenyReason string

const (
	denyReasonAllowed        egressDenyReason = ""
	denyReasonMetadataHost   egressDenyReason = "cloud-metadata-endpoint"
	denyReasonLinkLocal      egressDenyReason = "link-local-address"
	denyReasonUnspecified    egressDenyReason = "unspecified-address"
	denyReasonMulticast      egressDenyReason = "multicast-or-broadcast-address"
	denyReasonIPv6Literal    egressDenyReason = "ipv6-literal-target"
	denyReasonResolverPort   egressDenyReason = "name-resolution-port"
	denyReasonOutsideAllowed egressDenyReason = "target-outside-slot-allowlist"
)

func (r egressDenyReason) denied() bool { return r != denyReasonAllowed }

// Fixed metadata addresses. 169.254.169.254 is also link-local, but the more
// specific reason is reported so an operator sees what was actually attempted.
var deniedMetadataAddresses = []netip.Addr{
	netip.MustParseAddr("169.254.169.254"), // EC2 / GCE / Azure IMDS
	netip.MustParseAddr("169.254.170.2"),   // ECS task metadata
	netip.MustParseAddr("100.100.100.200"), // Alibaba Cloud metadata
	netip.MustParseAddr("fd00:ec2::254"),   // IMDSv2 over IPv6
}

// Lower-cased, trailing dot already stripped by the caller.
var deniedMetadataHosts = map[string]struct{}{
	"metadata.google.internal":   {},
	"metadata.goog":              {},
	"metadata":                   {},
	"instance-data":              {},
	"instance-data.ec2.internal": {},
}

// Ports that carry name resolution. DNS-over-HTTPS shares 443 and therefore
// cannot be separated here; it is denied by the allowlist instead, and the
// residual risk is recorded in the P3 design rather than claimed as closed.
var deniedResolverPorts = map[uint16]struct{}{
	53:   {}, // DNS over UDP/TCP
	853:  {}, // DNS over TLS
	5353: {}, // mDNS
}

// classifyEgressTarget reports why a target host/port pair is structurally
// refused, or denyReasonAllowed when only the slot allowlist governs it.
func classifyEgressTarget(host string, port uint16) egressDenyReason {
	if _, denied := deniedResolverPorts[port]; denied {
		return denyReasonResolverPort
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		if _, denied := deniedMetadataHosts[host]; denied {
			return denyReasonMetadataHost
		}
		return denyReasonAllowed
	}
	for _, metadata := range deniedMetadataAddresses {
		if address.Unmap() == metadata.Unmap() {
			return denyReasonMetadataHost
		}
	}
	unmapped := address.Unmap()
	switch {
	case unmapped.IsUnspecified():
		return denyReasonUnspecified
	case unmapped.IsLinkLocalUnicast(), unmapped.IsLinkLocalMulticast():
		return denyReasonLinkLocal
	case unmapped.IsMulticast(), unmapped.Is4() && unmapped == netip.MustParseAddr("255.255.255.255"):
		return denyReasonMulticast
	case unmapped.Is6(), address.Is4In6():
		// The fixed upstream egress path is IPv4-only. An IPv6 literal — including
		// the ::ffff: mapped form — would bypass the IPv4 allowlist comparisons
		// rather than extend them.
		return denyReasonIPv6Literal
	}
	return denyReasonAllowed
}
