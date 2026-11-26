package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type tarFile struct {
	hdr  *tar.Header
	data []byte
}

// readAr parses an ar archive (the format arWrite writes).
func readAr(t *testing.T, b []byte) map[string][]byte {
	t.Helper()
	if !bytes.HasPrefix(b, []byte("!<arch>\n")) {
		t.Fatal("kein ar-Archiv")
	}
	b = b[8:]
	out := map[string][]byte{}
	var order []string
	for len(b) > 0 {
		if len(b) < 60 || string(b[58:60]) != "`\n" {
			t.Fatalf("kaputter ar-Kopf: %q", b[:min(60, len(b))])
		}
		name := strings.TrimSpace(string(b[:16]))
		size, err := strconv.Atoi(strings.TrimSpace(string(b[48:58])))
		if err != nil {
			t.Fatal(err)
		}
		out[name] = b[60 : 60+size]
		order = append(order, name)
		b = b[60+size+size%2:]
	}
	if strings.Join(order, ",") != "debian-binary,control.tar.gz,data.tar.gz" {
		t.Fatalf("Reihenfolge der Einträge: %v", order)
	}
	return out
}

func readTarGz(t *testing.T, b []byte) map[string]tarFile {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	out := map[string]tarFile{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		out[h.Name] = tarFile{h, data}
	}
}

func write(t *testing.T, name, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(name), 0o755)
	if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBuildPackage(t *testing.T) {
	root := t.TempDir()
	dist := filepath.Join(root, "dist")
	write(t, filepath.Join(dist, "linux-amd64", "bastion-server"), "SERVER")
	write(t, filepath.Join(dist, "linux-amd64", "bastion-agent"), "AGENT-AMD64")
	write(t, filepath.Join(dist, "linux-amd64", "VERSION"), "1.2.3\r\n")
	write(t, filepath.Join(dist, "windows-amd64", "bastion-agent.exe"), "AGENT-WIN")
	write(t, filepath.Join(dist, "windows-amd64", "VERSION"), "1.2.3\r\n")
	write(t, filepath.Join(root, "README.md"), "readme")
	write(t, filepath.Join(root, "wiki.md"), "wiki")
	write(t, filepath.Join(root, "rescue", "linux", "build.sh"), "#!/bin/bash\r\necho hi\r\n")
	write(t, filepath.Join(root, "rescue", "winpe", "build.ps1"), "Write-Host hi\r\n")
	for _, f := range []string{"bastion-server.service", "default", "postinst", "prerm", "postrm"} {
		write(t, filepath.Join(root, "packaging", "deb", f), "#!/bin/sh\r\n# "+f+"\r\n")
	}

	mtime := time.Unix(1700000000, 0)
	p, err := buildPkg(options{Version: "1.2.3", Arch: "amd64", Dist: dist, Root: root, MTime: mtime})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := p.Write(&buf); err != nil {
		t.Fatal(err)
	}
	ar := readAr(t, buf.Bytes())
	if string(ar["debian-binary"]) != "2.0\n" {
		t.Fatalf("debian-binary: %q", ar["debian-binary"])
	}

	data := readTarGz(t, ar["data.tar.gz"])
	want := map[string]int64{
		"./opt/bastion-server/bastion-server":                         0o755,
		"./opt/bastion-server/VERSION":                                0o644,
		"./opt/bastion-server/README.md":                              0o644,
		"./opt/bastion-server/rescue/linux/build.sh":                  0o755,
		"./opt/bastion-server/rescue/winpe/build.ps1":                 0o644,
		"./opt/bastion-server/agents/linux-amd64/bastion-agent":       0o755,
		"./opt/bastion-server/agents/windows-amd64/bastion-agent.exe": 0o755,
		"./usr/lib/systemd/system/bastion-server.service":             0o644,
		"./etc/default/bastion-server":                                0o644,
		"./opt/bastion-server/":                                       0o755,
		"./usr/lib/systemd/":                                          0o755,
		"./":                                                          0o755,
	}
	for name, mode := range want {
		f, ok := data[name]
		if !ok {
			t.Errorf("fehlt im Paket: %s", name)
			continue
		}
		if f.hdr.Mode != mode {
			t.Errorf("%s: Rechte %o, erwartet %o", name, f.hdr.Mode, mode)
		}
		if f.hdr.Uid != 0 || f.hdr.Gid != 0 || f.hdr.Uname != "root" {
			t.Errorf("%s: Besitzer %d/%d %s", name, f.hdr.Uid, f.hdr.Gid, f.hdr.Uname)
		}
		if !f.hdr.ModTime.Equal(mtime) {
			t.Errorf("%s: mtime %v", name, f.hdr.ModTime)
		}
	}
	if _, ok := data["./opt/bastion-server/agents/linux-arm64/bastion-agent"]; ok {
		t.Error("nicht vorhandener Agent im Paket")
	}
	if l := data["./usr/bin/bastion-server"]; l.hdr == nil || l.hdr.Typeflag != tar.TypeSymlink || l.hdr.Linkname != "/opt/bastion-server/bastion-server" {
		t.Errorf("Symlink /usr/bin/bastion-server: %+v", l.hdr)
	}
	for _, name := range []string{"./opt/bastion-server/rescue/linux/build.sh", "./opt/bastion-server/VERSION",
		"./usr/lib/systemd/system/bastion-server.service", "./etc/default/bastion-server"} {
		if bytes.Contains(data[name].data, []byte("\r")) {
			t.Errorf("%s enthält CR", name)
		}
	}
	// parents come before their children
	seen := map[string]bool{}
	gz, _ := gzip.NewReader(bytes.NewReader(ar["data.tar.gz"]))
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		name := path.Clean(h.Name)
		if parent := path.Dir(name); name != "." && !seen[parent] {
			t.Errorf("%s vor seinem Ordner %s", h.Name, parent)
		}
		seen[name] = true
	}

	ctl := readTarGz(t, ar["control.tar.gz"])
	control := string(ctl["./control"].data)
	for _, line := range []string{"Package: bastion-server\n", "Version: 1.2.3\n", "Architecture: amd64\n",
		"Installed-Size: ", "Description: Bastion Backup - zentraler Management-Server\n", "\n .\n"} {
		if !strings.Contains(control, line) {
			t.Errorf("control ohne %q:\n%s", line, control)
		}
	}
	if string(ctl["./conffiles"].data) != "/etc/default/bastion-server\n" {
		t.Errorf("conffiles: %q", ctl["./conffiles"].data)
	}
	for _, s := range []string{"./postinst", "./prerm", "./postrm"} {
		f := ctl[s]
		if f.hdr == nil || f.hdr.Mode != 0o755 || bytes.Contains(f.data, []byte("\r")) {
			t.Errorf("%s fehlt, falsche Rechte oder CR", s)
		}
	}
	// md5sums lists every regular file with the right sum
	sums := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(ctl["./md5sums"].data))
	for sc.Scan() {
		sum, name, _ := strings.Cut(sc.Text(), "  ")
		sums[name] = sum
	}
	for name, f := range data {
		if f.hdr.Typeflag != tar.TypeReg {
			continue
		}
		rel := strings.TrimPrefix(name, "./")
		if sums[rel] != fmt.Sprintf("%x", md5.Sum(f.data)) {
			t.Errorf("md5sums falsch für %s", rel)
		}
	}
}

func TestBuildPackageNeedsServer(t *testing.T) {
	_, err := buildPkg(options{Version: "1.0.0", Arch: "arm64", Dist: t.TempDir(), Root: t.TempDir()})
	if err == nil {
		t.Fatal("Paket ohne Server-Programm gebaut")
	}
}
