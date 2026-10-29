// bastion-agent runs on every protected machine and in the rescue system.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"

	"bastion/internal/agent"
	"bastion/internal/proto"
	"bastion/internal/rescue"
)

const usage = `Bastion Backup Agent %s

Verwendung:
  bastion-agent install --server https://server:8443 --token TOKEN [--fingerprint SHA256]
  bastion-agent enroll --server ... --token ... [--fingerprint ...]   nur anmelden, kein Dienst
  bastion-agent uninstall
  bastion-agent run                 Agent im Vordergrund ausführen (sonst als Dienst)
  bastion-agent status
  bastion-agent rescue [--auto]     Image-Wiederherstellung im Rettungssystem
  bastion-agent version
`

func main() {
	if len(os.Args) < 2 {
		if agent.IsService() {
			runAgent()
			return
		}
		fmt.Printf(usage, proto.Version)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "enroll":
		fs := flag.NewFlagSet(cmd, flag.ExitOnError)
		server := fs.String("server", "", "Server-Adresse")
		token := fs.String("token", "", "Anmelde-Token")
		fp := fs.String("fingerprint", "", "SHA256-Fingerabdruck des Server-Zertifikats")
		fs.Parse(args)
		if err := agent.Enroll(*server, *token, *fp); err != nil {
			fmt.Fprintln(os.Stderr, "Fehler:", err)
			os.Exit(1)
		}
	case "install", "svc-install":
		fs := flag.NewFlagSet(cmd, flag.ExitOnError)
		server := fs.String("server", "", "Server-Adresse")
		token := fs.String("token", "", "Anmelde-Token")
		fp := fs.String("fingerprint", "", "SHA256-Fingerabdruck des Server-Zertifikats")
		force := fs.Bool("force", false, "neu anmelden, auch wenn bereits angemeldet")
		fs.Parse(args)
		install := func() error { return agent.Install(*server, *token, *fp, *force) }
		if cmd == "svc-install" {
			// started by the server as a temporary Windows service (push install)
			logf, _ := os.Create(filepath.Join(os.TempDir(), "bastion-install.log"))
			if logf != nil {
				os.Stdout = logf
				os.Stderr = logf
			}
			err := agent.RunService("BastionInstall", func(context.Context) error {
				err := install()
				if err != nil {
					fmt.Println("FEHLER:", err)
				} else {
					fmt.Println("OK")
				}
				return err
			})
			if err != nil {
				fmt.Println("FEHLER:", err)
			}
			return
		}
		if err := install(); err != nil {
			fmt.Fprintln(os.Stderr, "Fehler:", err)
			os.Exit(1)
		}
	case "uninstall":
		agent.Goodbye("auf dem Rechner deinstalliert")
		if err := agent.Uninstall(false); err != nil {
			fmt.Fprintln(os.Stderr, "Fehler:", err)
			os.Exit(1)
		}
		fmt.Println("Bastion-Agent entfernt.")
	case "run":
		runAgent()
	case "status":
		cfg, err := agent.LoadConfig()
		if err != nil {
			fmt.Println("nicht angemeldet:", err)
			os.Exit(1)
		}
		fmt.Printf("Server:  %s\nAgent:   %s\nVersion: %s (%s/%s)\n", cfg.Server, cfg.AgentID, proto.Version, runtime.GOOS, runtime.GOARCH)
	case "rescue":
		fs := flag.NewFlagSet(cmd, flag.ExitOnError)
		auto := fs.Bool("auto", false, "Auftrag von der Festplatte ohne Rückfragen ausführen")
		fs.Parse(args)
		if err := rescue.Run(*auto); err != nil {
			fmt.Fprintln(os.Stderr, "Fehler:", err)
			os.Exit(1)
		}
	case "version":
		fmt.Println(proto.Version)
	default:
		fmt.Printf(usage, proto.Version)
		os.Exit(2)
	}
}

// rotatingLog keeps the log file below 10 MB.
type rotatingLog struct {
	path string
	f    *os.File
	n    int64
}

func (r *rotatingLog) Write(p []byte) (int, error) {
	if r.f == nil || r.n > 10<<20 {
		if r.f != nil {
			r.f.Close()
			os.Rename(r.path, r.path+".1")
		}
		f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return len(p), nil
		}
		st, _ := f.Stat()
		r.f, r.n = f, st.Size()
	}
	n, err := r.f.Write(p)
	r.n += int64(n)
	return n, err
}

func runAgent() {
	os.MkdirAll(agent.StateDir(), 0o700)
	var out io.Writer = &rotatingLog{path: filepath.Join(agent.StateDir(), "agent.log")}
	if !agent.IsService() {
		out = io.MultiWriter(os.Stderr, out)
	}
	logger := log.New(out, "", log.LstdFlags)
	cfg, err := agent.LoadConfig()
	if err != nil {
		logger.Fatalf("Keine Konfiguration (%v). Zuerst 'bastion-agent install' ausführen.", err)
	}
	a := agent.New(cfg, logger)
	if agent.IsService() {
		if err := agent.RunService(agent.ServiceName, a.Run); err != nil {
			logger.Fatal(err)
		}
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	a.Run(ctx)
}
