// Package client is the HTTPS client agents and rescue environments use to
// talk to the server. The server certificate is pinned by fingerprint.
package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"bastion/internal/proto"
	"bastion/internal/repo"
)

type Client struct {
	Base string
	Auth string
	HTTP *http.Client
}

// NormalizeFingerprint turns "SHA256:AB:CD..." into "abcd...".
func NormalizeFingerprint(fp string) string {
	fp = strings.ToLower(strings.TrimSpace(fp))
	fp = strings.TrimPrefix(fp, "sha256:")
	return strings.ReplaceAll(fp, ":", "")
}

// Fingerprint returns the pin of a DER certificate.
func Fingerprint(der []byte) string {
	h := sha256.Sum256(der)
	return hex.EncodeToString(h[:])
}

func New(server, fingerprint, auth string) *Client {
	fp := NormalizeFingerprint(fingerprint)
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if fp != "" {
		tlsCfg.InsecureSkipVerify = true // replaced by the pin check below
		tlsCfg.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 || Fingerprint(raw[0]) != fp {
				return errors.New("Server-Zertifikat passt nicht zum hinterlegten Fingerabdruck")
			}
			return nil
		}
	}
	tr := &http.Transport{
		TLSClientConfig:     tlsCfg,
		DialContext:         (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
		Proxy:               http.ProxyFromEnvironment,
	}
	return &Client{Base: strings.TrimRight(server, "/"), Auth: auth, HTTP: &http.Client{Transport: tr}}
}

// HTTPError carries the server's status code.
type HTTPError struct {
	Status int
	Msg    string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("Server: %d %s", e.Status, e.Msg) }

func (c *Client) Do(ctx context.Context, method, path string, body io.Reader, ctype string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, body)
	if err != nil {
		return nil, err
	}
	if c.Auth != "" {
		req.Header.Set("Authorization", "Bearer "+c.Auth)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, &HTTPError{Status: resp.StatusCode, Msg: strings.TrimSpace(string(b))}
	}
	return resp, nil
}

// JSON sends in (if not nil) and decodes the response into out (if not nil).
// It returns false when the server answered 204 No Content.
func (c *Client) JSON(ctx context.Context, method, path string, in, out any) (bool, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return false, err
		}
		body = bytes.NewReader(b)
	}
	resp, err := c.Do(ctx, method, path, body, "application/json")
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return false, nil
	}
	if out != nil {
		return true, json.NewDecoder(resp.Body).Decode(out)
	}
	io.Copy(io.Discard, resp.Body)
	return true, nil
}

// GetChunk downloads and decompresses a chunk, verifying its hash.
func (c *Client) GetChunk(ctx context.Context, id repo.ID) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
		resp, err := c.Do(ctx, "GET", "/api/agent/chunks/"+id.String(), nil, "")
		if err != nil {
			lastErr = err
			var he *HTTPError
			if errors.As(err, &he) && he.Status == 404 {
				return nil, err
			}
			continue
		}
		z, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		b, err := repo.Decompress(z)
		if err != nil {
			lastErr = err
			continue
		}
		if repo.Hash(b) != id {
			lastErr = fmt.Errorf("chunk %s: Prüfsumme falsch", id)
			continue
		}
		return b, nil
	}
	return nil, lastErr
}

func (c *Client) Snapshot(ctx context.Context, id string) (*repo.Snapshot, error) {
	var s repo.Snapshot
	_, err := c.JSON(ctx, "GET", "/api/agent/snapshots/"+id, nil, &s)
	return &s, err
}

func (c *Client) Progress(ctx context.Context, job int64, p proto.Progress) error {
	_, err := c.JSON(ctx, "POST", fmt.Sprintf("/api/agent/jobs/%d/progress", job), p, nil)
	return err
}

func (c *Client) Result(ctx context.Context, job int64, r proto.JobResult) error {
	var err error
	for i := 0; i < 5; i++ {
		if _, err = c.JSON(ctx, "POST", fmt.Sprintf("/api/agent/jobs/%d/result", job), r, nil); err == nil {
			return nil
		}
		time.Sleep(3 * time.Second)
	}
	return err
}
