//go:build windows

package agent

import (
	"fmt"
	"net"
	"unsafe"

	"golang.org/x/sys/windows"

	"bastion/internal/proto"
)

const (
	gaaFlagIncludeGateways = 0x80
	ipAdapterDHCPEnabled   = 0x4
	ifOperStatusUp         = 1
)

// netConfigs returns the IPv4 setup of all connected adapters.
func netConfigs() []proto.NetConfig {
	size := uint32(16 << 10)
	var aa *windows.IpAdapterAddresses
	for i := 0; i < 3; i++ {
		buf := make([]byte, size)
		aa = (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0]))
		err := windows.GetAdaptersAddresses(windows.AF_INET, gaaFlagIncludeGateways, 0, aa, &size)
		if err == nil {
			break
		}
		if err != windows.ERROR_BUFFER_OVERFLOW {
			return nil
		}
		aa = nil
	}
	var out []proto.NetConfig
	for a := aa; a != nil; a = a.Next {
		if a.OperStatus != ifOperStatusUp || a.PhysicalAddressLength != 6 {
			continue
		}
		c := proto.NetConfig{MAC: net.HardwareAddr(a.PhysicalAddress[:6]).String(), DHCP: a.Flags&ipAdapterDHCPEnabled != 0}
		for u := a.FirstUnicastAddress; u != nil; u = u.Next {
			if ip := u.Address.IP().To4(); ip != nil && !ip.IsLinkLocalUnicast() && !ip.IsLoopback() {
				c.IPs = append(c.IPs, fmt.Sprintf("%s/%d", ip, u.OnLinkPrefixLength))
			}
		}
		for g := a.FirstGatewayAddress; g != nil; g = g.Next {
			if ip := g.Address.IP().To4(); ip != nil {
				c.Gateway = ip.String()
				break
			}
		}
		for d := a.FirstDnsServerAddress; d != nil; d = d.Next {
			if ip := d.Address.IP().To4(); ip != nil {
				c.DNS = append(c.DNS, ip.String())
			}
		}
		if len(c.IPs) > 0 {
			out = append(out, c)
		}
	}
	return out
}
