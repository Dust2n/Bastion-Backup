package fsx

import "testing"

func TestPathGuard(t *testing.T) {
	var g pathGuard
	for _, ok := range []string{"", "a", "a/b.txt", "Users/x/..datei"} {
		if err := g.check(ok); err != nil {
			t.Errorf("%q abgelehnt: %v", ok, err)
		}
	}
	for _, bad := range []string{"..", "../x", "a/../../x", "/etc/passwd", "a/\x00b"} {
		if err := g.check(bad); err == nil {
			t.Errorf("%q angenommen", bad)
		}
	}
	g.addLink("lib")
	if err := g.check("lib/evil"); err == nil {
		t.Error("Eintrag unterhalb einer Verknüpfung angenommen")
	}
	if err := g.check("library/ok"); err != nil {
		t.Errorf("Nachbarordner abgelehnt: %v", err)
	}
	if err := g.check("lib"); err != nil {
		t.Errorf("Verknüpfung selbst abgelehnt: %v", err)
	}
}
