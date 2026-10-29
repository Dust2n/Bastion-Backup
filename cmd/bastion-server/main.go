// bastion-server is the central backup management server.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"bastion/internal/agent"
	"bastion/internal/proto"
	"bastion/internal/server"
)

func defaultDataDir() string {
	if runtime.GOOS == "windows" {
		pd := os.Getenv("ProgramData")
		if pd == "" {
			pd = `C:\ProgramData`
		}
		return filepath.Join(pd, "BastionServer")
	}
	return "/var/lib/bastion-server"
}

// restore sets up the data directory of a new server from a server backup.
func restore(args []string) {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	from := fs.String("from", "", "Server-Sicherung (.bsb) oder Ordner eines Speicherorts")
	data := fs.String("data", defaultDataDir(), "Datenverzeichnis des neuen Servers")
	force := fs.Bool("force", false, "vorhandene Datenbank überschreiben")
	fs.Parse(args)
	if *from == "" {
		fmt.Fprintln(os.Stderr, "Verwendung: bastion-server restore -from <Datei oder Ordner> [-data <Datenverzeichnis>] [-force]")
		os.Exit(2)
	}
	file, err := server.FindServerBackup(*from)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Fehler:", err)
		os.Exit(1)
	}
	fmt.Println("Server-Sicherung:", file)
	pass := os.Getenv("BASTION_RESTORE_PASSPHRASE")
	if pass == "" {
		fmt.Print("Passwort der Server-Sicherung: ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		pass = strings.TrimRight(line, "\r\n")
	}
	if err := server.RestoreServerBackup(file, *data, pass, *force, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "Fehler:", err)
		os.Exit(1)
	}
	fmt.Println("Jetzt den Server starten bzw. den Dienst einrichten: bastion-server -install-service -data", *data)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "restore" {
		restore(os.Args[2:])
		return
	}
	data := flag.String("data", defaultDataDir(), "Datenverzeichnis (Datenbank, Repository, Schlüssel)")
	listen := flag.String("listen", ":8443", "HTTPS-Adresse")
	pxe := flag.Bool("pxe", false, "PXE-Dienste (TFTP, proxyDHCP, HTTP :8080) starten")
	plainHTTP := flag.String("http", "", "zusätzlich unverschlüsseltes HTTP, z. B. 127.0.0.1:8080 hinter einem Reverse-Proxy")
	installSvc := flag.Bool("install-service", false, "als Windows-Dienst bzw. systemd-Unit einrichten")
	version := flag.Bool("version", false, "Version ausgeben")
	flag.Parse()
	if *version {
		fmt.Println(proto.Version)
		return
	}
	if *installSvc {
		if err := installService(*data, *listen, *pxe, *plainHTTP); err != nil {
			fmt.Fprintln(os.Stderr, "Fehler:", err)
			os.Exit(1)
		}
		fmt.Println("Dienst eingerichtet und gestartet.")
		return
	}

	os.MkdirAll(*data, 0o700)
	logf, err := os.OpenFile(filepath.Join(*data, "server.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	var out io.Writer = os.Stderr
	if err == nil {
		// file first: as a service there is no console and writing to
		// stderr fails, which would stop MultiWriter before the file
		out = io.MultiWriter(logf, os.Stderr)
	}
	logger := log.New(out, "", log.LstdFlags)

	s, err := server.New(server.Options{DataDir: *data, Listen: *listen, PXE: *pxe, HTTP: *plainHTTP}, logger)
	if err != nil {
		logger.Fatal(err)
	}
	run := func(ctx context.Context) error { return s.Run(ctx) }
	if serviceMode() {
		if err := agent.RunService("BastionServer", run); err != nil {
			logger.Fatal(err)
		}
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		logger.Fatal(err)
	}
}
