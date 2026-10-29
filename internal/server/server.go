// Package server is the central management server: web UI, admin API,
// agent API, scheduler, chunk repository, rollout and PXE.
package server

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"bastion/internal/client"
	"bastion/internal/repo"
)

//go:embed web
var webFS embed.FS

type Options struct {
	DataDir string
	Listen  string // e.g. ":8443"
	PXE     bool
	// HTTP additionally serves plain HTTP, for a TLS-terminating reverse
	// proxy in front of the server (bind it to localhost).
	HTTP string
}

type Server struct {
	opt     Options
	db      *sql.DB
	repoKey []byte
	log     *log.Logger

	storeMu    sync.Mutex
	stores     map[int64]*repo.Store
	agentStore map[string]int64 // agent → storage of its running job
	moving     map[int64]bool   // plans whose backups are being moved
	signKey    ed25519.PrivateKey
	vault      []byte // key for stored credentials
	tlsCert    tls.Certificate
	fp         string

	wakeMu sync.Mutex
	wake   map[string]chan struct{}

	gcMu      sync.Mutex
	gcRunning bool

	trees *treeCache
	codes *rateLimiter
	login *rateLimiter
}

func (s *Server) path(p ...string) string {
	return filepath.Join(append([]string{s.opt.DataDir}, p...)...)
}

// loadKey reads a key file or creates it with gen().
func loadKey(path string, gen func() []byte) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		return b, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	b = gen()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return b, os.WriteFile(path, b, 0o600)
}

func random(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func New(opt Options, logger *log.Logger) (*Server, error) {
	s := &Server{opt: opt, log: logger, wake: map[string]chan struct{}{},
		stores: map[int64]*repo.Store{}, agentStore: map[string]int64{}, moving: map[int64]bool{},
		trees: newTreeCache(3), codes: newRateLimiter(10, time.Minute), login: newRateLimiter(10, 5*time.Minute)}
	for _, d := range []string{"", "keys", "tls", "agents", "rescue", "pxe", "snapshots", "cache"} {
		if err := os.MkdirAll(s.path(d), 0o700); err != nil {
			return nil, err
		}
	}
	var err error
	if s.db, err = openDB(s.path("bastion.db")); err != nil {
		return nil, fmt.Errorf("Datenbank: %w", err)
	}

	repoKey, err := loadKey(s.path("keys", "repo.key"), func() []byte { return random(32) })
	if err != nil {
		return nil, err
	}
	if env := os.Getenv("BASTION_REPO_KEY"); env != "" {
		if repoKey, err = base64.StdEncoding.DecodeString(env); err != nil {
			return nil, fmt.Errorf("BASTION_REPO_KEY: %w", err)
		}
	}
	s.repoKey = repoKey
	seed, err := loadKey(s.path("keys", "signing.key"), func() []byte { return random(ed25519.SeedSize) })
	if err != nil {
		return nil, err
	}
	s.signKey = ed25519.NewKeyFromSeed(seed)
	if s.vault, err = loadKey(s.path("keys", "vault.key"), func() []byte { return random(32) }); err != nil {
		return nil, err
	}
	if err := s.initStorages(); err != nil {
		return nil, fmt.Errorf("Speicherorte: %w", err)
	}
	if err := s.loadTLS(); err != nil {
		return nil, fmt.Errorf("TLS: %w", err)
	}
	if err := s.ensureAdmin(); err != nil {
		return nil, err
	}
	s.scanAgentBinaries()
	return s, nil
}

// loadTLS uses tls/cert.pem + key.pem or creates a self-signed certificate.
// Agents pin its fingerprint.
func (s *Server) loadTLS() error {
	certPath, keyPath := s.path("tls", "cert.pem"), s.path("tls", "key.pem")
	if _, err := os.Stat(certPath); errors.Is(err, fs.ErrNotExist) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return err
		}
		host, _ := os.Hostname()
		tmpl := &x509.Certificate{
			SerialNumber: new(big.Int).SetBytes(random(16)),
			Subject:      pkix.Name{CommonName: host, Organization: []string{"Bastion Backup"}},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().AddDate(20, 0, 0),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			DNSNames:     []string{host, "localhost"},
		}
		if fqdn, err := net.LookupCNAME(host); err == nil && fqdn != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, strings.TrimSuffix(fqdn, "."))
		}
		addrs, _ := net.InterfaceAddrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				tmpl.IPAddresses = append(tmpl.IPAddresses, ipn.IP)
			}
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if err != nil {
			return err
		}
		kb, _ := x509.MarshalECPrivateKey(key)
		os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
		os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
		s.log.Printf("Selbstsigniertes Zertifikat erstellt: %s", certPath)
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return err
	}
	s.tlsCert = cert
	s.fp = client.Fingerprint(cert.Certificate[0])
	return nil
}

// PublicURL is the address agents use.
func (s *Server) PublicURL() string {
	if u := s.setting("public_url", ""); u != "" {
		return strings.TrimRight(u, "/")
	}
	host, _ := os.Hostname()
	port := s.opt.Listen
	if i := strings.LastIndex(port, ":"); i >= 0 {
		port = port[i+1:]
	}
	return fmt.Sprintf("https://%s:%s", host, port)
}

func (s *Server) Fingerprint() string { return s.fp }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.agentRoutes(mux)
	s.adminRoutes(mux)
	sub, _ := fs.Sub(webFS, "web")
	static := http.FileServer(http.FS(sub))
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:")
		static.ServeHTTP(w, r)
	}))
	return logRequests(s.log, mux)
}

func logRequests(l *log.Logger, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				l.Printf("panic %s %s: %v", r.Method, r.URL.Path, v)
				http.Error(w, "interner Fehler", 500)
			}
		}()
		// Read the (bounded) body up front: if a handler answers without
		// reading it, Go closes the connection for clients that sent
		// "Expect: 100-continue" (e.g. PowerShell), breaking keep-alive.
		if r.Body != nil && r.ContentLength != 0 {
			body, err := io.ReadAll(io.LimitReader(r.Body, 17<<20))
			if err != nil {
				http.Error(w, "Anfrage unvollständig", 400)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		h.ServeHTTP(w, r)
	})
}

// Run serves HTTPS and starts background tasks until ctx ends.
func (s *Server) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.opt.Listen,
		Handler:           s.Handler(),
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{s.tlsCert}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          log.New(discardTLSNoise{s.log}, "", 0),
	}
	go s.scheduler(ctx)
	go func() {
		s.importNewerSnapshots()
		s.updateRepoStats()
		s.indexMissing()
	}()
	if s.opt.PXE || s.setting("pxe_enabled", "") == "1" {
		go s.runPXE(ctx)
	}
	var plain *http.Server
	if s.opt.HTTP != "" {
		plain = &http.Server{Addr: s.opt.HTTP, Handler: srv.Handler, ReadHeaderTimeout: 30 * time.Second}
		s.log.Printf("Zusätzlich unverschlüsseltes HTTP auf %s (nur hinter einem Reverse-Proxy verwenden)", s.opt.HTTP)
		go func() {
			if err := plain.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.log.Printf("HTTP: %v", err)
			}
		}()
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
		if plain != nil {
			plain.Shutdown(sctx)
		}
	}()
	s.log.Printf("Bastion-Server läuft auf %s", s.PublicURL())
	s.log.Printf("Zertifikats-Fingerabdruck: %s", s.fp)
	err := srv.ListenAndServeTLS("", "")
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

type discardTLSNoise struct{ l *log.Logger }

func (d discardTLSNoise) Write(p []byte) (int, error) {
	if !strings.Contains(string(p), "TLS handshake error") {
		d.l.Print(string(p))
	}
	return len(p), nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

func httpErr(w http.ResponseWriter, code int, format string, args ...any) {
	http.Error(w, fmt.Sprintf(format, args...), code)
}

// notifyAgent wakes up a long-polling agent.
func (s *Server) notifyAgent(id string) {
	s.wakeMu.Lock()
	if ch, ok := s.wake[id]; ok {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	s.wakeMu.Unlock()
}

func (s *Server) wakeChan(id string) chan struct{} {
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	ch, ok := s.wake[id]
	if !ok {
		ch = make(chan struct{}, 1)
		s.wake[id] = ch
	}
	return ch
}

type rateLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string][]time.Time
}

func newRateLimiter(max int, window time.Duration) *rateLimiter {
	return &rateLimiter{max: max, window: window, hits: map[string][]time.Time{}}
}

func (r *rateLimiter) allow(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	cut := time.Now().Add(-r.window)
	h := r.hits[key][:0]
	for _, t := range r.hits[key] {
		if t.After(cut) {
			h = append(h, t)
		}
	}
	if len(h) >= r.max {
		r.hits[key] = h
		return false
	}
	r.hits[key] = append(h, time.Now())
	return true
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
