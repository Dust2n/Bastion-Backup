//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseUnitHandInstalled(t *testing.T) {
	unit := "[Service]\nExecStart=/usr/local/bin/bastion-server -data /srv/bastion -listen :8443\nRestart=always\n"
	exe, data := parseUnit(unit, func(string) map[string]string { return nil })
	if exe != "/usr/local/bin/bastion-server" || data != "/srv/bastion" {
		t.Fatalf("exe=%q data=%q", exe, data)
	}
}

func TestParseUnitPackage(t *testing.T) {
	unit, err := os.ReadFile(filepath.Join("..", "..", "packaging", "deb", "bastion-server.service"))
	if err != nil {
		t.Fatal(err)
	}
	def := filepath.Join(t.TempDir(), "default")
	os.WriteFile(def, []byte("# Kommentar\nBASTION_DATA=\"/data/bastion\"\nBASTION_OPTS=\"-listen :9443\"\n"), 0o644)
	read := func(name string) map[string]string {
		if name != "/etc/default/bastion-server" {
			t.Errorf("EnvironmentFile %q", name)
		}
		return readEnvFile(def)
	}
	exe, data := parseUnit(string(unit), read)
	if exe != "/opt/bastion-server/bastion-server" || data != "/data/bastion" {
		t.Fatalf("exe=%q data=%q", exe, data)
	}
	// without /etc/default/bastion-server: the default from Environment=
	exe, data = parseUnit(string(unit), func(string) map[string]string { return nil })
	if data != "/var/lib/bastion-server" {
		t.Fatalf("data=%q", data)
	}
}

func TestSplitQuoted(t *testing.T) {
	got := splitQuoted(`A=1 "B=x y" C=`)
	if len(got) != 3 || got[0] != "A=1" || got[1] != "B=x y" || got[2] != "C=" {
		t.Fatalf("%q", got)
	}
}
