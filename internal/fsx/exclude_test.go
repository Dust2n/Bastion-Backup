package fsx

import "testing"

func TestExcludeBelow(t *testing.T) {
	cases := []struct {
		root, abs string
		fold      bool
		want      string
		ok        bool
	}{
		{`D:\`, `D:\Bastion`, true, "Bastion/*", true},
		{`D:\`, `d:\Bastion\`, true, "Bastion/*", true},
		{`C:\`, `C:\BastionData\repo`, true, "BastionData/repo/*", true},
		{`C:\`, `D:\Bastion`, true, "", false},
		{`D:\`, `D:\`, true, "*", true},
		{`D:\Daten`, `D:\Datenbank`, true, "", false}, // prefix of a name, not a parent
		{`D:\Daten`, `D:\Daten\repo`, true, "repo/*", true},
		{"/", "/srv/bastion/repo", false, "srv/bastion/repo/*", true},
		{"/srv", "/srv/bastion", false, "bastion/*", true},
		{"/srv", "/Srv/bastion", false, "", false},
		{"/home", "/srv/bastion", false, "", false},
	}
	for _, c := range cases {
		got, ok := ExcludeBelow(c.root, c.abs, c.fold)
		if got != c.want || ok != c.ok {
			t.Errorf("ExcludeBelow(%q, %q) = %q, %v; erwartet %q, %v", c.root, c.abs, got, ok, c.want, c.ok)
		}
		if ok && got != "*" {
			// the pattern must leave out what lies below, not the directory itself
			dir := got[:len(got)-2]
			if !Excluded(dir+"/chunks/ab/cd", []string{got}, c.fold) || Excluded(dir, []string{got}, c.fold) {
				t.Errorf("Muster %q schließt nicht wie erwartet aus", got)
			}
		}
	}
}
