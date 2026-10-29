//go:build windows

package rescue

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

func netsh(args ...string) (string, error) {
	cmd := exec.Command("netsh", append([]string{"interface", "ipv4"}, args...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		return s, fmt.Errorf("netsh %s: %s", strings.Join(args, " "), s)
	}
	return s, nil
}

// netsh accepts the interface index as name.
func setDHCP(i netIf) error {
	idx := fmt.Sprintf("name=%d", i.Index)
	netsh("set", "address", idx, "source=dhcp")    // fails harmlessly when DHCP is already on
	netsh("set", "dnsservers", idx, "source=dhcp") // same
	exec.Command("ipconfig", "/renew").Run()
	return nil
}

func setStatic(i netIf, ip net.IP, prefix int, gw net.IP, dns []string) error {
	idx := fmt.Sprintf("name=%d", i.Index)
	mask := net.IP(net.CIDRMask(prefix, 32)).String()
	args := []string{"set", "address", idx, "source=static", "address=" + ip.String(), "mask=" + mask}
	if gw != nil {
		args = append(args, "gateway="+gw.String(), "gwmetric=1")
	}
	if _, err := netsh(args...); err != nil {
		return err
	}
	if len(dns) == 0 {
		netsh("set", "dnsservers", idx, "source=static", "address=none", "validate=no")
		return nil
	}
	if _, err := netsh("set", "dnsservers", idx, "source=static", "address="+dns[0], "register=none", "validate=no"); err != nil {
		return err
	}
	for n, d := range dns[1:] {
		netsh("add", "dnsservers", idx, "address="+d, fmt.Sprintf("index=%d", n+2), "validate=no")
	}
	return nil
}

// hideInput switches off console echo until the returned func is called.
func hideInput() func() {
	h := windows.Handle(os.Stdin.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) != nil {
		return func() {}
	}
	windows.SetConsoleMode(h, mode&^windows.ENABLE_ECHO_INPUT)
	return func() { windows.SetConsoleMode(h, mode) }
}
