//go:build linux

package rescue

import (
	"fmt"
	"net"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

func setDHCP(i netIf) error {
	sh("dhclient", "-r", i.Name)
	sh("ip", "addr", "flush", "dev", i.Name)
	sh("ip", "link", "set", i.Name, "up")
	_, err := sh("dhclient", "-1", "-timeout", "25", i.Name)
	return err
}

func setStatic(i netIf, ip net.IP, prefix int, gw net.IP, dns []string) error {
	sh("dhclient", "-r", i.Name)
	sh("ip", "addr", "flush", "dev", i.Name)
	if _, err := sh("ip", "link", "set", i.Name, "up"); err != nil {
		return err
	}
	if _, err := sh("ip", "addr", "add", fmt.Sprintf("%s/%d", ip, prefix), "dev", i.Name); err != nil {
		return err
	}
	if gw != nil {
		if _, err := sh("ip", "route", "replace", "default", "via", gw.String(), "dev", i.Name); err != nil {
			return err
		}
	}
	if len(dns) > 0 {
		var b strings.Builder
		for _, d := range dns {
			b.WriteString("nameserver " + d + "\n")
		}
		return os.WriteFile("/etc/resolv.conf", []byte(b.String()), 0o644)
	}
	return nil
}

// hideInput switches off terminal echo until the returned func is called.
func hideInput() func() {
	t, err := unix.IoctlGetTermios(0, unix.TCGETS)
	if err != nil {
		return func() {}
	}
	old := *t
	t.Lflag &^= unix.ECHO
	unix.IoctlSetTermios(0, unix.TCSETS, t)
	return func() { unix.IoctlSetTermios(0, unix.TCSETS, &old) }
}
