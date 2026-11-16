package server

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"bastion/internal/proto"
)

// Server update from a release folder:
//
//	bastion-server update -from <folder>
//
// The folder is what build.ps1 writes to .\dist or an unpacked release
// archive: one sub-folder per platform (windows-amd64, linux-amd64, ...)
// with bastion-server, bastion-agent and VERSION, optionally a rescue
// folder and the documentation. A single platform folder works as well.
// ApplyUpdate only replaces files; restarting the service is up to the
// caller, because the running process can not restart itself portably.

// UpdatePlan describes what ApplyUpdate did.
type UpdatePlan struct {
	Version  string
	Agents   []string // platforms whose agent package was published
	Server   bool     // server binary replaced
	Support  []string // support files copied next to the server
	Previous string   // the replaced binary, kept for a rollback
}

func binName(base, platform string) string {
	if strings.HasPrefix(platform, "windows-") {
		return base + ".exe"
	}
	return base
}

// copyFileAtomic writes dst via a temporary file, so a crash never leaves
// a half-written program behind.
func copyFileAtomic(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".new"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// releasePlatforms finds the platform folders of a release.
func releasePlatforms(from string) (map[string]string, error) {
	out := map[string]string{}
	entries, err := os.ReadDir(from)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(from, e.Name(), binName("bastion-agent", e.Name()))); err == nil {
			out[e.Name()] = filepath.Join(from, e.Name())
		}
	}
	if len(out) == 0 {
		// a single platform folder (unpacked release archive)
		for _, p := range []string{runtime.GOOS + "-" + runtime.GOARCH} {
			if _, err := os.Stat(filepath.Join(from, binName("bastion-agent", p))); err == nil {
				out[p] = from
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s enthält kein Bastion-Release (erwartet z. B. %s%c%s)", from,
			runtime.GOOS+"-"+runtime.GOARCH, filepath.Separator, binName("bastion-agent", runtime.GOOS+"-"+runtime.GOARCH))
	}
	return out, nil
}

// ApplyUpdate publishes the agent packages of the release in dataDir,
// copies the support files next to exe and replaces exe with the server
// program of this platform. logw receives one line per step.
func ApplyUpdate(from, dataDir, exe string, logw io.Writer) (*UpdatePlan, error) {
	plats, err := releasePlatforms(from)
	if err != nil {
		return nil, err
	}
	own := runtime.GOOS + "-" + runtime.GOARCH
	ownDir, ok := plats[own]
	if !ok {
		return nil, fmt.Errorf("das Release enthält nichts für diese Plattform (%s)", own)
	}
	newExe := filepath.Join(ownDir, binName("bastion-server", own))
	if _, err := os.Stat(newExe); err != nil {
		return nil, fmt.Errorf("Server-Programm fehlt im Release: %s", newExe)
	}
	plan := &UpdatePlan{}
	if b, err := os.ReadFile(filepath.Join(ownDir, "VERSION")); err == nil {
		plan.Version = strings.TrimSpace(string(b))
	}

	// 1. agent packages: the server offers them to the machines after its restart
	for p, dir := range plats {
		name := binName("bastion-agent", p)
		dst := filepath.Join(dataDir, "agents", p)
		if err := copyFileAtomic(filepath.Join(dir, name), filepath.Join(dst, name), 0o755); err != nil {
			return plan, fmt.Errorf("Agent %s: %w", p, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "VERSION")); err == nil {
			if err := copyFileAtomic(filepath.Join(dir, "VERSION"), filepath.Join(dst, "VERSION"), 0o644); err != nil {
				return plan, fmt.Errorf("Agent %s: %w", p, err)
			}
		}
		plan.Agents = append(plan.Agents, p)
		fmt.Fprintf(logw, "Agent-Paket %s bereitgestellt\n", p)
	}

	// 2. support files next to the server (rescue build scripts, documentation)
	appDir := filepath.Dir(exe)
	for _, base := range []string{from, filepath.Join(from, "support"), ownDir} {
		for _, name := range []string{"README.md", "wiki.md", "VERBESSERUNGEN.md", "VERSION"} {
			src := filepath.Join(base, name)
			if fi, err := os.Stat(src); err == nil && !fi.IsDir() {
				if err := copyFileAtomic(src, filepath.Join(appDir, name), 0o644); err != nil {
					return plan, err
				}
				plan.Support = append(plan.Support, name)
			}
		}
		rescue := filepath.Join(base, "rescue")
		if fi, err := os.Stat(rescue); err == nil && fi.IsDir() {
			err := filepath.WalkDir(rescue, func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				rel, _ := filepath.Rel(base, p)
				return copyFileAtomic(p, filepath.Join(appDir, rel), 0o755)
			})
			if err != nil {
				return plan, fmt.Errorf("Rettungsskripte: %w", err)
			}
			plan.Support = append(plan.Support, "rescue")
		}
	}
	if len(plan.Support) > 0 {
		fmt.Fprintf(logw, "Begleitdateien aktualisiert: %s\n", strings.Join(plan.Support, ", "))
	}

	// 3. the server program. A running program can not be overwritten on
	// Windows, but it can be renamed: the old one stays as .old (also the
	// way back), the new one takes its name.
	if same, _ := sameFile(newExe, exe); same {
		fmt.Fprintln(logw, "Server-Programm ist bereits das aus dem Release")
		return plan, nil
	}
	old := exe + ".old"
	if _, err := os.Stat(exe); err == nil {
		os.Remove(old)
		if err := os.Rename(exe, old); err != nil {
			return plan, fmt.Errorf("altes Server-Programm beiseitelegen: %w", err)
		}
		plan.Previous = old
	}
	if err := copyFileAtomic(newExe, exe, 0o755); err != nil {
		// put the old one back, the server must stay startable
		if plan.Previous != "" {
			os.Rename(old, exe)
		}
		return plan, fmt.Errorf("Server-Programm ersetzen: %w", err)
	}
	plan.Server = true
	fmt.Fprintf(logw, "Server-Programm ersetzt: %s (vorheriges: %s)\n", exe, old)
	return plan, nil
}

func sameFile(a, b string) (bool, error) {
	fa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(fa, fb), nil
}

// afterVersionChange runs once after the server came up in a new version:
// agents are updated if the administrator asked for that, and a WinPE
// rescue system that exists is rebuilt, because it carries a copy of the
// agent.
func (s *Server) afterVersionChange() {
	prev := s.setting("server_version", "")
	if prev == proto.Version {
		return
	}
	s.setSetting("server_version", proto.Version)
	if prev == "" {
		return // first start of a version that records it
	}
	s.audit("Server", "version", prev+" → "+proto.Version)
	if s.setting("auto_update_agents", "") == "1" {
		n := s.queueAgentUpdates("", "Server-Update")
		s.log.Printf("Server-Update %s → %s: %d Agents erhalten das Update", prev, proto.Version, n)
	}
	if s.setting("auto_rescue_build", "1") != "0" {
		if _, err := os.Stat(s.path("rescue", "winpe.wim")); err == nil {
			if id, err := s.startRescueBuild("Server-Update"); err != nil {
				s.log.Printf("Rettungssystem nicht neu gebaut: %v", err)
			} else {
				s.log.Printf("Rettungssystem wird mit dem neuen Agent neu gebaut (Auftrag %d)", id)
			}
		}
	}
}

// queueAgentUpdates queues an update for every agent (of a group) that
// runs an older version than the published package.
func (s *Server) queueAgentUpdates(grp, by string) int {
	rows, err := s.db.Query(`SELECT id FROM agents WHERE removed=0 AND (?='' OR grp=?)`, grp, grp)
	if err != nil {
		return 0
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	n := 0
	for _, id := range ids {
		if _, err := s.updateJob(id, by); err == nil {
			n++
		}
	}
	return n
}
