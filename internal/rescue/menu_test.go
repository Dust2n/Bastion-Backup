package rescue

import "testing"

func TestParseIPv4(t *testing.T) {
	cases := []struct {
		in     string
		ip     string
		prefix int
		ok     bool
	}{
		{"192.168.1.50/24", "192.168.1.50", 24, true},
		{"10.0.0.5 255.255.0.0", "10.0.0.5", 16, true},
		{"172.16.50.99", "172.16.50.99", 24, true},
		{"172.16.50.99/33", "", 0, false},
		{"300.1.1.1", "", 0, false},
	}
	for _, c := range cases {
		ip, p, err := parseIPv4(c.in)
		if (err == nil) != c.ok {
			t.Fatalf("%q: err=%v", c.in, err)
		}
		if c.ok && (ip.String() != c.ip || p != c.prefix) {
			t.Fatalf("%q: got %s/%d", c.in, ip, p)
		}
	}
}
