//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func serviceMode() bool { return false }

func installService(data, listen string, pxe bool, plainHTTP string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{exe, "-data", data, "-listen", listen}
	if pxe {
		args = append(args, "-pxe")
	}
	if plainHTTP != "" {
		args = append(args, "-http", plainHTTP)
	}
	unit := fmt.Sprintf(`[Unit]
Description=Bastion Backup Server
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=%s
Restart=always
RestartSec=5
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
`, strings.Join(args, " "))
	if err := os.WriteFile("/etc/systemd/system/bastion-server.service", []byte(unit), 0o644); err != nil {
		return err
	}
	exec.Command("systemctl", "daemon-reload").Run()
	if out, err := exec.Command("systemctl", "enable", "--now", "bastion-server").CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}

const unitFile = "/etc/systemd/system/bastion-server.service"

// unitFiles in the order systemd prefers them: the one -install-service
// writes, then the one of the Debian package.
var unitFiles = []string{unitFile, "/usr/lib/systemd/system/bastion-server.service", "/lib/systemd/system/bastion-server.service"}

// installedService returns program and data directory from the systemd unit.
func installedService() (exe, data string, ok bool) {
	for _, f := range unitFiles {
		if b, err := os.ReadFile(f); err == nil {
			exe, data = parseUnit(string(b), readEnvFile)
			return exe, data, true
		}
	}
	return "", "", false
}

// parseUnit reads program and data directory from ExecStart. Variables
// (${BASTION_DATA} in the unit of the package) come from Environment= and
// EnvironmentFile=, the latter overriding the former as in systemd.
func parseUnit(unit string, envFile func(string) map[string]string) (exe, data string) {
	env := map[string]string{}
	var files []string
	var exec []string
	for _, line := range strings.Split(unit, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "Environment="):
			for _, kv := range splitQuoted(strings.TrimPrefix(line, "Environment=")) {
				if k, v, ok := strings.Cut(kv, "="); ok {
					env[k] = v
				}
			}
		case strings.HasPrefix(line, "EnvironmentFile="):
			files = append(files, strings.TrimPrefix(strings.TrimPrefix(line, "EnvironmentFile="), "-"))
		case strings.HasPrefix(line, "ExecStart=") && exec == nil:
			exec = strings.Fields(strings.TrimPrefix(line, "ExecStart="))
		}
	}
	for _, f := range files {
		for k, v := range envFile(f) {
			env[k] = v
		}
	}
	expand := func(s string) string {
		return os.Expand(s, func(k string) string { return env[k] })
	}
	if len(exec) == 0 {
		return "", ""
	}
	exe = expand(exec[0])
	for i := 1; i+1 < len(exec); i++ {
		if exec[i] == "-data" || exec[i] == "--data" {
			data = expand(exec[i+1])
		}
	}
	return exe, data
}

// splitQuoted splits at spaces outside double quotes and removes the quotes.
func splitQuoted(s string) []string {
	var out []string
	var cur strings.Builder
	quoted, have := false, false
	for _, r := range s {
		switch {
		case r == '"':
			quoted, have = !quoted, true
		case r == ' ' && !quoted:
			if have {
				out = append(out, cur.String())
			}
			cur.Reset()
			have = false
		default:
			cur.WriteRune(r)
			have = true
		}
	}
	if have {
		out = append(out, cur.String())
	}
	return out
}

// readEnvFile reads KEY=value lines as systemd's EnvironmentFile= does.
func readEnvFile(name string) map[string]string {
	env := map[string]string{}
	b, err := os.ReadFile(name)
	if err != nil {
		return env
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		env[strings.TrimSpace(k)] = v
	}
	return env
}

// packageManaged reports whether the program belongs to the Debian package.
func packageManaged(exe string) bool {
	b, err := os.ReadFile("/var/lib/dpkg/info/bastion-server.list")
	if err != nil {
		return false
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	for _, line := range strings.Split(string(b), "\n") {
		if line == exe {
			return true
		}
	}
	return false
}

func restartService() error {
	if out, err := exec.Command("systemctl", "restart", "bastion-server").CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}
