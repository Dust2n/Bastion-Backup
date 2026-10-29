//go:build linux

package agent

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strings"

	"bastion/internal/proto"
)

// netConfigs returns the IPv4 setup of all connected adapters. Whether an
// address came from DHCP is not known on Linux, so DHCP is reported as
// true: the rescue system tries DHCP first and uses the addresses only
// as a fallback.
func netConfigs() []proto.NetConfig {
	gws := defaultGateways()
	dns := resolvers()
	ifs, _ := net.Interfaces()
	var out []proto.NetConfig
	for _, i := range ifs {
		if i.Flags&net.FlagLoopback != 0 || i.Flags&net.FlagUp == 0 || len(i.HardwareAddr) != 6 {
			continue
		}
		c := proto.NetConfig{MAC: i.HardwareAddr.String(), DHCP: true, Gateway: gws[i.Name], DNS: dns}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLinkLocalUnicast() {
				ones, _ := ipn.Mask.Size()
				c.IPs = append(c.IPs, fmt.Sprintf("%s/%d", ipn.IP, ones))
			}
		}
		if len(c.IPs) > 0 {
			out = append(out, c)
		}
	}
	return out
}

// defaultGateways reads the default routes per interface from /proc/net/route.
func defaultGateways() map[string]string {
	out := map[string]string{}
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fl := strings.Fields(sc.Text())
		if len(fl) < 3 || fl[1] != "00000000" {
			continue
		}
		b, err := hex.DecodeString(fl[2])
		if err != nil || len(b) != 4 {
			continue
		}
		ip := make(net.IP, 4)
		binary.BigEndian.PutUint32(ip, binary.LittleEndian.Uint32(b))
		out[fl[0]] = ip.String()
	}
	return out
}

func resolvers() []string {
	for _, p := range []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var out []string
		for _, l := range strings.Split(string(b), "\n") {
			fl := strings.Fields(l)
			if len(fl) >= 2 && fl[0] == "nameserver" {
				if ip := net.ParseIP(fl[1]).To4(); ip != nil && !ip.IsLoopback() {
					out = append(out, ip.String())
				}
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}
