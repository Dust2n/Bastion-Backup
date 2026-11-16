package server

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// Building the WinPE rescue system on the server itself. It needs the
// Windows ADK with the WinPE add-on on the server and the build script
// next to the server program (rescue\winpe\build.ps1, installed by the
// setup and by "bastion-server update"). Drivers the rescue system needs
// for the machines' disks and network cards (e.g. VirtIO) are taken from
// <data>\rescue\drivers, if that folder exists.

const jobRescueBuild = "rescue_build"

var rescueBuildMu sync.Mutex

// appDir is the folder of the server program (overridden in tests).
var appDir = func() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

// rescueBuildCommand creates the build process (overridden in tests).
var rescueBuildCommand = func(script, serverURL, fingerprint, agentExe, out, drivers string) (*exec.Cmd, error) {
	if runtime.GOOS != "windows" {
		return nil, errors.New("das WinPE-Rettungssystem lässt sich nur auf einem Windows-Server bauen")
	}
	args := []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script,
		"-Server", serverURL, "-Fingerprint", fingerprint, "-Agent", agentExe, "-Out", out}
	if drivers != "" {
		args = append(args, "-Drivers", drivers)
	}
	return exec.Command("powershell.exe", args...), nil
}

func (s *Server) rescueBuildScript() (string, error) {
	p := filepath.Join(appDir(), "rescue", "winpe", "build.ps1")
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("Build-Skript nicht gefunden (%s)", p)
	}
	return p, nil
}

// startRescueBuild starts the build as a server-side job.
func (s *Server) startRescueBuild(by string) (int64, error) {
	script, err := s.rescueBuildScript()
	if err != nil {
		return 0, err
	}
	agentExe := s.path("agents", "windows-amd64", "bastion-agent.exe")
	if _, err := os.Stat(agentExe); err != nil {
		return 0, errors.New("kein Windows-Agent unter agents\\windows-amd64 bereitgestellt")
	}
	out := s.path("cache", fmt.Sprintf("rescue-build-%d", time.Now().UnixNano()))
	drivers := s.path("rescue", "drivers")
	if fi, err := os.Stat(drivers); err != nil || !fi.IsDir() {
		drivers = ""
	}
	cmd, err := rescueBuildCommand(script, s.PublicURL(), s.fp, agentExe, out, drivers)
	if err != nil {
		return 0, err
	}
	if !rescueBuildMu.TryLock() {
		return 0, errors.New("das Rettungssystem wird bereits gebaut")
	}
	res, err := s.db.Exec(`INSERT INTO jobs(agent_id,type,params,status,created,started,updated,created_by,message) VALUES('',?,'{}','running',?,?,?,?,?)`,
		jobRescueBuild, now(), now(), now(), by, "WinPE-Rettungssystem wird gebaut")
	if err != nil {
		rescueBuildMu.Unlock()
		return 0, err
	}
	id, _ := res.LastInsertId()
	s.publishJob(id)
	s.bg("Rettungssystem", func() {
		defer rescueBuildMu.Unlock()
		s.runRescueBuild(id, cmd, out, drivers != "")
	})
	return id, nil
}

func (s *Server) runRescueBuild(job int64, cmd *exec.Cmd, out string, withDrivers bool) {
	defer os.RemoveAll(out)
	logf := func(format string, args ...any) {
		s.appendLog(job, []string{time.Now().Format("15:04:05 ") + fmt.Sprintf(format, args...)})
	}
	finish := func(status, msg string) {
		logf("%s", msg)
		s.db.Exec(`UPDATE jobs SET status=?,message=?,finished=?,percent=CASE WHEN ?='success' THEN 100 ELSE percent END WHERE id=?`,
			status, msg, now(), status, job)
		s.publishJob(job)
		if status == "failed" {
			s.bg("notifyFailure", func() { s.notifyFailure(job) })
		}
	}
	if withDrivers {
		logf("Treiber aus %s", s.path("rescue", "drivers"))
	} else {
		logf("Ohne zusätzliche Treiber (Ordner %s fehlt)", s.path("rescue", "drivers"))
	}
	os.MkdirAll(out, 0o700)
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			if line := sc.Text(); line != "" {
				logf("%s", line)
				s.db.Exec(`UPDATE jobs SET message=?,updated=? WHERE id=?`, line, now(), job)
				s.publishJob(job)
			}
		}
		io.Copy(io.Discard, pr)
	}()
	err := cmd.Run()
	pw.Close()
	<-done
	if err != nil {
		finish("failed", fmt.Sprintf("Build fehlgeschlagen: %v", err))
		return
	}
	// publish only complete results; the old files stay until then
	files := []string{"winpe.wim", "bastion-rescue-winpe.iso"}
	for _, f := range files {
		fi, err := os.Stat(filepath.Join(out, f))
		if err != nil || fi.Size() < 1<<20 {
			finish("failed", fmt.Sprintf("Build ohne Ergebnis: %s fehlt oder ist unvollständig", f))
			return
		}
	}
	for _, f := range files {
		if err := copyFileAtomic(filepath.Join(out, f), s.path("rescue", f), 0o644); err != nil {
			finish("failed", fmt.Sprintf("%s veröffentlichen: %v", f, err))
			return
		}
	}
	ver, _ := os.ReadFile(s.path("agents", "windows-amd64", "VERSION"))
	s.setSetting("rescue_built", jsonStr(map[string]any{"time": now(), "agent": string(bytesTrim(ver))}))
	finish("success", "Rettungssystem gebaut und bereitgestellt (winpe.wim, bastion-rescue-winpe.iso)")
}

// canBuildRescue tells the web UI whether the build button makes sense.
func (s *Server) canBuildRescue() bool {
	if runtime.GOOS != "windows" {
		return false
	}
	_, err := s.rescueBuildScript()
	return err == nil
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func bytesTrim(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}
