// Package agent is the service running on every protected machine.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"bastion/internal/client"
	"bastion/internal/layout"
	"bastion/internal/platform"
	"bastion/internal/proto"
)

// Config is stored next to the agent with restrictive permissions.
type Config struct {
	Server      string `json:"server"`
	Fingerprint string `json:"fingerprint"`
	AgentID     string `json:"agent_id"`
	Secret      string `json:"secret"`
	SigningKey  string `json:"signing_key"`
}

func LoadConfig() (*Config, error) {
	b, err := os.ReadFile(ConfigPath())
	if err != nil {
		return nil, err
	}
	var c Config
	return &c, json.Unmarshal(b, &c)
}

func SaveConfig(c *Config) error {
	p := ConfigPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		return err
	}
	secureDir(filepath.Dir(p))
	return nil
}

func (c *Config) Client() *client.Client {
	return client.New(c.Server, c.Fingerprint, "agent:"+c.AgentID+":"+c.Secret)
}

type Agent struct {
	cfg    *Config
	c      *client.Client
	log    *log.Logger
	layout *layout.Machine
	layAt  time.Time
	job    int64
}

func New(cfg *Config, logger *log.Logger) *Agent {
	return &Agent{cfg: cfg, c: cfg.Client(), log: logger}
}

func (a *Agent) info() proto.AgentInfo {
	host, _ := os.Hostname()
	ips, macs := platform.NetInfo()
	inf := proto.AgentInfo{
		Hostname: host, OS: runtime.GOOS, OSVersion: platform.OSVersion(), Arch: runtime.GOARCH,
		Version: proto.Version, IPs: ips, MACs: macs, RunningJob: a.job,
	}
	if time.Since(a.layAt) > 6*time.Hour {
		if m, err := platform.CaptureLayout(); err == nil {
			a.layout = m
			inf.Layout = m
		} else {
			a.log.Printf("Layout: %v", err)
		}
		a.layAt = time.Now()
	}
	return inf
}

// Run polls the server for jobs until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	a.log.Printf("Bastion-Agent %s verbindet mit %s", proto.Version, a.cfg.Server)
	a.afterImageRestore(ctx)
	refreshProgramEntry()
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		var job proto.Job
		pctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		ok, err := a.c.JSON(pctx, "POST", "/api/agent/poll", a.info(), &job)
		cancel()
		if err != nil {
			var he *client.HTTPError
			if errors.As(err, &he) && he.Status == 401 {
				a.log.Printf("Server lehnt diesen Agent ab (entfernt?): %v", err)
				backoff = 5 * time.Minute
			} else {
				a.log.Printf("Server nicht erreichbar: %v", err)
			}
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
			}
			if backoff < time.Minute {
				backoff *= 2
			}
			continue
		}
		backoff = 5 * time.Second
		if ok && job.ID != 0 {
			a.runJob(ctx, job)
		}
	}
	return nil
}

// reporter sends progress every few seconds and receives cancel requests.
type reporter struct {
	a      *Agent
	job    int64
	mu     sync.Mutex
	p      proto.Progress
	logs   []string
	newLog []string
	cancel func()
}

func (r *reporter) Set(f func(p *proto.Progress)) {
	r.mu.Lock()
	f(&r.p)
	r.mu.Unlock()
}

func (r *reporter) Log(format string, args ...any) {
	msg := time.Now().Format("15:04:05 ") + fmt.Sprintf(format, args...)
	r.a.log.Print(msg)
	r.mu.Lock()
	if len(r.logs) < 1000 {
		r.logs = append(r.logs, msg)
		r.newLog = append(r.newLog, msg)
	}
	r.mu.Unlock()
}

func (r *reporter) send(ctx context.Context) {
	r.mu.Lock()
	p := r.p
	p.Log = r.newLog
	r.newLog = nil
	r.mu.Unlock()
	var reply proto.ProgressReply
	if _, err := r.a.c.JSON(ctx, "POST", fmt.Sprintf("/api/agent/jobs/%d/progress", r.job), p, &reply); err == nil && reply.Cancel {
		r.Log("Abbruch durch Administrator")
		r.cancel()
	}
}

func (r *reporter) loop(ctx context.Context) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			r.send(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (a *Agent) runJob(ctx context.Context, job proto.Job) {
	a.log.Printf("Auftrag %d (%s) gestartet", job.ID, job.Type)
	a.job = job.ID
	defer func() { a.job = 0 }()
	jctx, cancel := context.WithCancel(ctx)
	defer cancel()
	rep := &reporter{a: a, job: job.ID, cancel: cancel}
	lctx, stopLoop := context.WithCancel(ctx)
	go rep.loop(lctx)

	var res proto.JobResult
	func() {
		defer func() {
			if r := recover(); r != nil {
				res = proto.JobResult{Message: fmt.Sprintf("interner Fehler: %v", r)}
			}
		}()
		switch job.Type {
		case proto.JobBackup:
			var p proto.BackupParams
			json.Unmarshal(job.Params, &p)
			res = a.backup(jctx, rep, p)
		case proto.JobRestoreFiles:
			var p proto.RestoreFilesParams
			json.Unmarshal(job.Params, &p)
			res = a.restoreFiles(jctx, rep, p)
		case proto.JobRestoreImage:
			var p proto.RestoreImageParams
			json.Unmarshal(job.Params, &p)
			res = a.stageImageRestore(jctx, rep, job.ID, p)
		case proto.JobUpdate:
			var p proto.UpdateParams
			json.Unmarshal(job.Params, &p)
			res = a.update(jctx, rep, p)
		case proto.JobUninstall:
			res = proto.JobResult{Success: true, Message: "Agent wird deinstalliert"}
		default:
			res = proto.JobResult{Message: "unbekannter Auftragstyp " + job.Type}
		}
	}()
	stopLoop()
	rep.send(ctx)
	rep.mu.Lock()
	res.Log = rep.newLog
	rep.mu.Unlock()
	if jctx.Err() != nil && ctx.Err() == nil && !res.Success {
		res.Message = "abgebrochen: " + res.Message
	}
	if err := a.c.Result(ctx, job.ID, res); err != nil {
		a.log.Printf("Ergebnis von Auftrag %d nicht übermittelt: %v", job.ID, err)
	}
	a.log.Printf("Auftrag %d beendet: %v %s", job.ID, res.Success, res.Message)

	switch {
	case job.Type == proto.JobUninstall:
		// the helper stops the service cleanly, so it is not restarted
		Goodbye("über die Weboberfläche deinstalliert")
		Uninstall(true)
	case job.Type == proto.JobUpdate && res.Success:
		// the service manager restarts the new binary
		os.Exit(3)
	case job.Type == proto.JobRestoreImage && res.Success && res.Waiting:
		var p proto.RestoreImageParams
		json.Unmarshal(job.Params, &p)
		msg := p.Message
		if msg == "" {
			msg = "Bastion: Der Computer wird für eine Systemwiederherstellung neu gestartet. Bitte Arbeit speichern."
		}
		if err := platform.ScheduleReboot(p.DelaySec, msg); err != nil {
			a.log.Printf("Neustart fehlgeschlagen: %v", err)
		}
	}
}
