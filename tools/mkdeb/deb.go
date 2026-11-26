package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/md5"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"
)

// entry is one file, directory or symlink of the package. Name is the
// absolute path on the target system ("/opt/bastion-server/...").
type entry struct {
	Name string
	Mode int64
	Data []byte
	Link string // symlink target
	Dir  bool
}

// pkg is the content of a binary package.
type pkg struct {
	Control   map[string]string // fields of DEBIAN/control
	Scripts   map[string][]byte // postinst, prerm, postrm, ...
	Conffiles []string
	Files     []entry
	MTime     time.Time
}

var controlOrder = []string{"Package", "Version", "Architecture", "Maintainer", "Installed-Size",
	"Depends", "Recommends", "Suggests", "Section", "Priority", "Homepage", "Description"}

// lf turns CRLF into LF. Git on Windows (core.autocrlf) checks text files
// out with CRLF; shell scripts and systemd units must not contain them.
func lf(b []byte) []byte { return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")) }

// withDirs adds the missing parent directories of all files.
func withDirs(files []entry) []entry {
	have := map[string]bool{}
	for _, f := range files {
		if f.Dir {
			have[f.Name] = true
		}
	}
	out := append([]entry(nil), files...)
	for _, f := range files {
		for d := path.Dir(f.Name); d != "/" && !have[d]; d = path.Dir(d) {
			have[d] = true
			out = append(out, entry{Name: d, Mode: 0o755, Dir: true})
		}
	}
	// parents before children, otherwise in a stable order
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func installedSize(files []entry) int64 {
	var n int64
	for _, f := range files {
		n += int64(len(f.Data))
	}
	return (n + 1023) / 1024
}

func (p *pkg) controlFile() []byte {
	var b strings.Builder
	for _, k := range controlOrder {
		v, ok := p.Control[k]
		if !ok || v == "" {
			continue
		}
		if k == "Description" {
			// first line is the synopsis, the rest is indented
			lines := strings.Split(strings.TrimRight(v, "\n"), "\n")
			fmt.Fprintf(&b, "Description: %s\n", lines[0])
			for _, l := range lines[1:] {
				if strings.TrimSpace(l) == "" {
					l = "."
				}
				fmt.Fprintf(&b, " %s\n", l)
			}
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n", k, v)
	}
	return []byte(b.String())
}

func tarGz(entries []entry, mtime time.Time) ([]byte, error) {
	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{
			Name:    "." + e.Name,
			Mode:    e.Mode,
			ModTime: mtime,
			Uname:   "root",
			Gname:   "root",
			Format:  tar.FormatGNU,
		}
		switch {
		case e.Dir:
			h.Typeflag = tar.TypeDir
			h.Name += "/"
		case e.Link != "":
			h.Typeflag = tar.TypeSymlink
			h.Linkname = e.Link
			h.Mode = 0o777
		default:
			h.Typeflag = tar.TypeReg
			h.Size = int64(len(e.Data))
		}
		if err := tw.WriteHeader(h); err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(e.Data); err != nil {
				return nil, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// arWrite writes a Unix ar archive as dpkg expects it.
func arWrite(w io.Writer, mtime time.Time, members ...arMember) error {
	if _, err := io.WriteString(w, "!<arch>\n"); err != nil {
		return err
	}
	for _, m := range members {
		hdr := fmt.Sprintf("%-16s%-12d%-6d%-6d%-8o%-10d`\n", m.name, mtime.Unix(), 0, 0, 0o100644, len(m.data))
		if len(hdr) != 60 {
			return fmt.Errorf("ar-Kopf für %s ist %d Byte lang", m.name, len(hdr))
		}
		if _, err := io.WriteString(w, hdr); err != nil {
			return err
		}
		if _, err := w.Write(m.data); err != nil {
			return err
		}
		if len(m.data)%2 == 1 {
			if _, err := w.Write([]byte{'\n'}); err != nil {
				return err
			}
		}
	}
	return nil
}

type arMember struct {
	name string
	data []byte
}

// Write writes the package as .deb.
func (p *pkg) Write(w io.Writer) error {
	files := withDirs(p.Files)
	if _, ok := p.Control["Installed-Size"]; !ok {
		p.Control["Installed-Size"] = fmt.Sprint(installedSize(files))
	}

	var sums strings.Builder
	for _, f := range files {
		if !f.Dir && f.Link == "" {
			fmt.Fprintf(&sums, "%x  %s\n", md5.Sum(f.Data), strings.TrimPrefix(f.Name, "/"))
		}
	}
	ctl := []entry{
		{Name: "/control", Mode: 0o644, Data: p.controlFile()},
		{Name: "/md5sums", Mode: 0o644, Data: []byte(sums.String())},
	}
	if len(p.Conffiles) > 0 {
		ctl = append(ctl, entry{Name: "/conffiles", Mode: 0o644, Data: []byte(strings.Join(p.Conffiles, "\n") + "\n")})
	}
	names := make([]string, 0, len(p.Scripts))
	for n := range p.Scripts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		ctl = append(ctl, entry{Name: "/" + n, Mode: 0o755, Data: lf(p.Scripts[n])})
	}
	ctlTar, err := tarGz(append([]entry{{Name: "", Mode: 0o755, Dir: true}}, ctl...), p.MTime)
	if err != nil {
		return err
	}
	dataTar, err := tarGz(append([]entry{{Name: "", Mode: 0o755, Dir: true}}, files...), p.MTime)
	if err != nil {
		return err
	}
	return arWrite(w, p.MTime,
		arMember{"debian-binary", []byte("2.0\n")},
		arMember{"control.tar.gz", ctlTar},
		arMember{"data.tar.gz", dataTar})
}
