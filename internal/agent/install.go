package agent

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"crypto/tls"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"bastion/internal/client"
	"bastion/internal/platform"
	"bastion/internal/proto"
)

// Install copies the agent into the install directory, enrolls it with
// the server (unless already enrolled) and registers the service.
func Install(server, token, fingerprint string, force bool) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	dst := filepath.Join(InstallDir(), exeName())
	cfg, _ := LoadConfig()
	if cfg == nil || cfg.AgentID == "" || force || (token != "" && cfg.Server != server) {
		if server == "" || token == "" {
			return errors.New("--server und --token sind für die Erstinstallation nötig")
		}
		cfg, err = enroll(server, token, fingerprint)
		if err != nil {
			return fmt.Errorf("Anmeldung am Server fehlgeschlagen: %w", diagnose(server, fingerprint, err))
		}
		if err := SaveConfig(cfg); err != nil {
			return err
		}
		fmt.Printf("Agent angemeldet als %s\n", cfg.AgentID)
	}
	if !samePath(self, dst) {
		StopService()
		if err := os.MkdirAll(InstallDir(), 0o755); err != nil {
			return err
		}
		if err := copyFile(self, dst); err != nil {
			return fmt.Errorf("Kopieren nach %s: %w", dst, err)
		}
	}
	if err := InstallService(dst); err != nil {
		return fmt.Errorf("Dienst einrichten: %w", err)
	}
	fmt.Println("Bastion-Agent installiert und gestartet.")
	return nil
}

// Enroll only registers with the server and stores the configuration
// (no service), e.g. for running the agent in the foreground.
func Enroll(server, token, fingerprint string) error {
	cfg, err := enroll(server, token, fingerprint)
	if err != nil {
		return err
	}
	fmt.Printf("Agent angemeldet als %s, Konfiguration: %s\n", cfg.AgentID, ConfigPath())
	return SaveConfig(cfg)
}

func enroll(server, token, fingerprint string) (*Config, error) {
	host, _ := os.Hostname()
	ips, macs := platform.NetInfo()
	c := client.New(server, fingerprint, "")
	var resp proto.EnrollResponse
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := c.JSON(ctx, "POST", "/api/agent/enroll", proto.EnrollRequest{
		Token: token, Hostname: host, OS: runtime.GOOS, Arch: runtime.GOARCH, Version: proto.Version, IPs: ips, MACs: macs,
	}, &resp)
	if err != nil {
		return nil, err
	}
	return &Config{Server: server, Fingerprint: client.NormalizeFingerprint(fingerprint), AgentID: resp.AgentID,
		Secret: resp.Secret, SigningKey: resp.SigningKey}, nil
}

// diagnose turns a failed enrollment into a message that says which step
// failed: name resolution, network connection or server certificate.
func diagnose(server, fingerprint string, orig error) error {
	u, err := url.Parse(server)
	if err != nil || u.Hostname() == "" {
		return fmt.Errorf("Server-Adresse %q ungültig", server)
	}
	host, port := u.Hostname(), u.Port()
	if port == "" {
		port = "443"
	}
	if net.ParseIP(host) == nil {
		if _, err := net.LookupHost(host); err != nil {
			return fmt.Errorf("Servername %s ist auf diesem Rechner nicht auflösbar (DNS prüfen oder IP-Adresse als Server-Adresse verwenden)", host)
		}
	}
	addr := net.JoinHostPort(host, port)
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("Server %s ist von diesem Rechner aus nicht erreichbar (%v) – Firewall auf dem Server (eingehend TCP %s) bzw. im Netz prüfen", addr, err, port)
	}
	tc := tls.Client(conn, &tls.Config{InsecureSkipVerify: true, ServerName: host})
	tc.SetDeadline(time.Now().Add(10 * time.Second))
	err = tc.Handshake()
	if err != nil {
		tc.Close()
		return fmt.Errorf("TLS-Verbindung zu %s fehlgeschlagen (%v) – Proxy oder SSL-Prüfung im Netz?", addr, err)
	}
	certs := tc.ConnectionState().PeerCertificates
	tc.Close()
	if fp := client.NormalizeFingerprint(fingerprint); fp != "" && len(certs) > 0 && client.Fingerprint(certs[0].Raw) != fp {
		return fmt.Errorf("Zertifikat von %s passt nicht zum erwarteten Fingerabdruck – ein Proxy/SSL-Prüfung fängt die Verbindung ab oder das Server-Zertifikat wurde erneuert", addr)
	}
	return orig
}

// Goodbye tells the server that this agent is being uninstalled, so it is
// shown as removed instead of offline. Errors are ignored: uninstalling
// must also work without the server.
func Goodbye(reason string) {
	cfg, err := LoadConfig()
	if err != nil || cfg.AgentID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg.Client().JSON(ctx, "POST", "/api/agent/goodbye", map[string]string{"reason": reason}, nil)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".new"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		os.Remove(dst + ".old")
		if err := os.Rename(dst, dst+".old"); err != nil {
			return err
		}
	}
	return os.Rename(tmp, dst)
}

func samePath(a, b string) bool {
	fa, err1 := os.Stat(a)
	fb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(fa, fb)
}

// update downloads, verifies and swaps the agent binary.
func (a *Agent) update(ctx context.Context, rep *reporter, p proto.UpdateParams) proto.JobResult {
	if p.Version == proto.Version {
		return proto.JobResult{Success: true, Message: "bereits Version " + p.Version}
	}
	pub, err := base64.StdEncoding.DecodeString(a.cfg.SigningKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return proto.JobResult{Message: "kein gültiger Signaturschlüssel hinterlegt"}
	}
	sig, _ := base64.StdEncoding.DecodeString(p.Signature)
	if !ed25519.Verify(pub, []byte(p.SHA256), sig) {
		return proto.JobResult{Message: "Signatur des Updates ungültig"}
	}
	rep.Log("Lade Version %s", p.Version)
	resp, err := a.c.Do(ctx, "GET", p.Path, nil, "")
	if err != nil {
		return proto.JobResult{Message: err.Error()}
	}
	defer resp.Body.Close()
	self, _ := os.Executable()
	tmp := self + ".download"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return proto.JobResult{Message: err.Error()}
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return proto.JobResult{Message: err.Error()}
	}
	if hex.EncodeToString(h.Sum(nil)) != p.SHA256 {
		os.Remove(tmp)
		return proto.JobResult{Message: "Prüfsumme des Updates falsch"}
	}
	os.Remove(self + ".old")
	if err := os.Rename(self, self+".old"); err != nil {
		return proto.JobResult{Message: err.Error()}
	}
	if err := os.Rename(tmp, self); err != nil {
		os.Rename(self+".old", self)
		return proto.JobResult{Message: err.Error()}
	}
	return proto.JobResult{Success: true, Message: "aktualisiert auf " + p.Version + ", Dienst startet neu"}
}
