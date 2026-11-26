// mkdeb builds the Debian package of the Bastion Backup Server from the
// output of build.ps1, without dpkg-deb (so it also runs on Windows).
//
//	go run ./tools/mkdeb -version 1.2.3 -arch amd64 -dist dist -out dist/release
//
// Result: <out>/bastion-server_<version>_<arch>.deb
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	appDir  = "/opt/bastion-server"
	pkgName = "bastion-server"
)

// agent platforms the server distributes (see build.ps1)
var agentPlatforms = []string{"windows-amd64", "linux-amd64", "linux-arm64"}

type options struct {
	Version string
	Arch    string // Debian architecture: amd64, arm64
	Dist    string // output of build.ps1
	Root    string // repository: README.md, wiki.md, rescue/, packaging/deb/
	MTime   time.Time
}

func readFile(name string) ([]byte, error) {
	b, err := os.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("fehlt: %w", err)
	}
	return b, nil
}

// buildPkg collects the package content.
func buildPkg(o options) (*pkg, error) {
	own := filepath.Join(o.Dist, "linux-"+o.Arch)
	p := &pkg{
		Control: map[string]string{
			"Package":      pkgName,
			"Version":      o.Version,
			"Architecture": o.Arch,
			"Maintainer":   "Bastion Backup",
			"Suggests":     "debootstrap, cpio, zstd, grub-pc-bin, grub-efi-amd64-bin, xorriso, mtools",
			"Section":      "admin",
			"Priority":     "optional",
			"Homepage":     "https://github.com/Dust2n/Bastion-Backup",
			"Description": "Bastion Backup - zentraler Management-Server\n" +
				"Selbst gehostete, zentral verwaltete Backup-Lösung für Windows- und\n" +
				"Linux-Rechner: Weboberfläche, Zeitpläne, deduplizierendes und\n" +
				"verschlüsseltes Repository, Verteilung und Updates der Agents,\n" +
				"Image-Restore über Rettungssysteme (WinPE, Linux, PXE).\n" +
				"\n" +
				"Der Server läuft als systemd-Dienst bastion-server. Einstellungen\n" +
				"stehen in /etc/default/bastion-server, die Daten standardmäßig in\n" +
				"/var/lib/bastion-server (bleiben beim Entfernen erhalten).",
		},
		Scripts:   map[string][]byte{},
		Conffiles: []string{"/etc/default/bastion-server"},
		MTime:     o.MTime,
	}
	add := func(name string, mode int64, data []byte) {
		p.Files = append(p.Files, entry{Name: name, Mode: mode, Data: data})
	}

	srv, err := readFile(filepath.Join(own, "bastion-server"))
	if err != nil {
		return nil, err
	}
	add(appDir+"/bastion-server", 0o755, srv)
	if b, err := os.ReadFile(filepath.Join(own, "VERSION")); err == nil {
		add(appDir+"/VERSION", 0o644, lf(b))
	}
	p.Files = append(p.Files, entry{Name: "/usr/bin/bastion-server", Link: appDir + "/bastion-server"})

	for _, doc := range []string{"README.md", "wiki.md"} {
		b, err := readFile(filepath.Join(o.Root, doc))
		if err != nil {
			return nil, err
		}
		add(appDir+"/"+doc, 0o644, lf(b))
	}

	// agents: postinst copies them into <data>/agents
	n := 0
	for _, plat := range agentPlatforms {
		name := "bastion-agent"
		if strings.HasPrefix(plat, "windows") {
			name += ".exe"
		}
		b, err := os.ReadFile(filepath.Join(o.Dist, plat, name))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Hinweis: Agent %s fehlt, wird nicht mitgeliefert\n", plat)
			continue
		}
		add(path.Join(appDir, "agents", plat, name), 0o755, b)
		if v, err := os.ReadFile(filepath.Join(o.Dist, plat, "VERSION")); err == nil {
			add(path.Join(appDir, "agents", plat, "VERSION"), 0o644, lf(v))
		}
		n++
	}
	if n == 0 {
		return nil, fmt.Errorf("keine Agents in %s gefunden (zuerst build.ps1 ausführen)", o.Dist)
	}

	// build scripts for the rescue systems, next to the program as
	// appDir() in internal/server expects them
	rescue := filepath.Join(o.Root, "rescue")
	err = filepath.WalkDir(rescue, func(fp string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(o.Root, fp)
		b, err := os.ReadFile(fp)
		if err != nil {
			return err
		}
		mode := int64(0o644)
		if strings.HasSuffix(fp, ".sh") {
			mode, b = 0o755, lf(b)
		}
		add(appDir+"/"+filepath.ToSlash(rel), mode, b)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("Rettungsskripte: %w", err)
	}

	pk := filepath.Join(o.Root, "packaging", "deb")
	unit, err := readFile(filepath.Join(pk, "bastion-server.service"))
	if err != nil {
		return nil, err
	}
	add("/usr/lib/systemd/system/bastion-server.service", 0o644, lf(unit))
	def, err := readFile(filepath.Join(pk, "default"))
	if err != nil {
		return nil, err
	}
	add("/etc/default/bastion-server", 0o644, lf(def))
	for _, s := range []string{"postinst", "prerm", "postrm"} {
		b, err := readFile(filepath.Join(pk, s))
		if err != nil {
			return nil, err
		}
		p.Scripts[s] = b
	}
	return p, nil
}

var debVersion = regexp.MustCompile(`^[0-9][A-Za-z0-9.+~-]*$`)

func mtimeFromEnv() (time.Time, error) {
	// reproducible builds: https://reproducible-builds.org/specs/source-date-epoch/
	if s := os.Getenv("SOURCE_DATE_EPOCH"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("SOURCE_DATE_EPOCH: %w", err)
		}
		return time.Unix(n, 0), nil
	}
	return time.Now().Truncate(time.Second), nil
}

func main() {
	var o options
	out := flag.String("out", filepath.Join("dist", "release"), "Zielordner")
	flag.StringVar(&o.Version, "version", "", "Version, z. B. 1.2.3")
	flag.StringVar(&o.Arch, "arch", "amd64", "Architektur: amd64 oder arm64")
	flag.StringVar(&o.Dist, "dist", "dist", "Ausgabe von build.ps1")
	flag.StringVar(&o.Root, "root", ".", "Repository (README.md, wiki.md, rescue/, packaging/deb/)")
	flag.Parse()

	fail := func(err error) {
		fmt.Fprintln(os.Stderr, "Fehler:", err)
		os.Exit(1)
	}
	if !debVersion.MatchString(o.Version) {
		fail(fmt.Errorf("ungültige Version %q (-version 1.2.3)", o.Version))
	}
	if o.Arch != "amd64" && o.Arch != "arm64" {
		fail(fmt.Errorf("unbekannte Architektur %q", o.Arch))
	}
	var err error
	if o.MTime, err = mtimeFromEnv(); err != nil {
		fail(err)
	}
	p, err := buildPkg(o)
	if err != nil {
		fail(err)
	}
	var buf bytes.Buffer
	if err := p.Write(&buf); err != nil {
		fail(err)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fail(err)
	}
	name := filepath.Join(*out, fmt.Sprintf("%s_%s_%s.deb", pkgName, o.Version, o.Arch))
	if err := os.WriteFile(name, buf.Bytes(), 0o644); err != nil {
		fail(err)
	}
	fmt.Println(name)
}
