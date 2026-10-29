// Package proto holds the types exchanged between agent and server.
package proto

import (
	"encoding/json"

	"bastion/internal/layout"
	"bastion/internal/repo"
)

// Version of server and agent. Set at build time with
// -ldflags "-X bastion/internal/proto.Version=1.2.3".
var Version = "0.1.0"

type EnrollRequest struct {
	Token    string   `json:"token"`
	Hostname string   `json:"hostname"`
	OS       string   `json:"os"`
	Arch     string   `json:"arch"`
	Version  string   `json:"version"`
	IPs      []string `json:"ips"`
	MACs     []string `json:"macs"`
}

type EnrollResponse struct {
	AgentID    string `json:"agent_id"`
	Secret     string `json:"secret"`
	SigningKey string `json:"signing_key"` // base64 ed25519 public key for updates
}

// AgentInfo is sent with every poll.
type AgentInfo struct {
	Hostname   string          `json:"hostname"`
	OS         string          `json:"os"`
	OSVersion  string          `json:"os_version"`
	Arch       string          `json:"arch"`
	Version    string          `json:"version"`
	IPs        []string        `json:"ips"`
	MACs       []string        `json:"macs"`
	Layout     *layout.Machine `json:"layout,omitempty"`
	RunningJob int64           `json:"running_job,omitempty"`
}

// Job types.
const (
	JobBackup       = "backup"
	JobRestoreFiles = "restore_files"
	JobRestoreImage = "restore_image"
	JobUpdate       = "update"
	JobUninstall    = "uninstall"
)

type Job struct {
	ID     int64           `json:"id"`
	Type   string          `json:"type"`
	Params json.RawMessage `json:"params"`
}

type BackupParams struct {
	PlanID        int64    `json:"plan_id"`
	Kind          string   `json:"kind"`            // image | files
	Paths         []string `json:"paths,omitempty"` // for kind=files
	Excludes      []string `json:"excludes,omitempty"`
	Parent        string   `json:"parent,omitempty"` // previous snapshot for incremental reading
	BandwidthKBps int      `json:"bandwidth_kbps,omitempty"`
	UseSnapshot   bool     `json:"use_snapshot"`
}

type RestoreFilesParams struct {
	SnapshotID string   `json:"snapshot_id"`
	Volume     string   `json:"volume"`
	Paths      []string `json:"paths"`               // relative paths inside the volume; empty = all
	Target     string   `json:"target,omitempty"`    // empty = original location
	Overwrite  string   `json:"overwrite,omitempty"` // always | never | rename
	// Format recreates the whole (data) volume before restoring it.
	Format bool `json:"format,omitempty"`
}

// ProgressReply tells the agent to cancel the running job.
type ProgressReply struct {
	Cancel bool `json:"cancel"`
}

type RestoreImageParams struct {
	SnapshotID string `json:"snapshot_id"`
	Token      string `json:"token"`          // one-time restore token for the rescue environment
	Code       string `json:"code,omitempty"` // short code typed in on USB/PXE rescue media
	DiskNumber int    `json:"disk_number"`    // -1 = original system disk
	DelaySec   int    `json:"delay_sec"`      // warning before reboot
	Message    string `json:"message,omitempty"`
}

type UpdateParams struct {
	Version   string `json:"version"`
	Path      string `json:"path"` // download path on the server
	SHA256    string `json:"sha256"`
	Signature string `json:"signature"` // base64 ed25519 over the sha256 hex string
}

// Progress is reported while a job runs.
type Progress struct {
	Percent float64  `json:"percent"`
	Files   int64    `json:"files"`
	Bytes   int64    `json:"bytes"`
	Total   int64    `json:"total"`
	Message string   `json:"message,omitempty"`
	Log     []string `json:"log,omitempty"`
}

type JobResult struct {
	Success    bool     `json:"success"`
	Message    string   `json:"message"`
	SnapshotID string   `json:"snapshot_id,omitempty"`
	Log        []string `json:"log,omitempty"`
	// Waiting means the job continues after a reboot (image restore).
	Waiting bool `json:"waiting,omitempty"`
}

type MissingRequest struct {
	IDs []repo.ID `json:"ids"`
}

type MissingResponse struct {
	Missing []repo.ID `json:"missing"`
}

// RestoreTicket is written to the local disk before rebooting into the
// rescue environment, or fetched with a code from USB/PXE media.
type RestoreTicket struct {
	Server      string `json:"server"`
	Fingerprint string `json:"fingerprint"`
	Token       string `json:"token"`
	JobID       int64  `json:"job_id"`
	SnapshotID  string `json:"snapshot_id"`
	DiskNumber  int    `json:"disk_number"`
	DiskSerial  string `json:"disk_serial,omitempty"`
	DiskSize    int64  `json:"disk_size,omitempty"`
	// Network is the IPv4 setup of the machine when the restore was
	// staged; the rescue system uses it when there is no DHCP.
	Network []NetConfig `json:"network,omitempty"`
}

// NetConfig is the IPv4 setup of one network adapter.
type NetConfig struct {
	MAC     string   `json:"mac"`
	DHCP    bool     `json:"dhcp"`
	IPs     []string `json:"ips"` // 192.168.1.20/24
	Gateway string   `json:"gateway,omitempty"`
	DNS     []string `json:"dns,omitempty"`
}

// RestoreInfo is returned to the rescue environment.
type RestoreInfo struct {
	JobID    int64          `json:"job_id"`
	Token    string         `json:"token"`
	Snapshot *repo.Snapshot `json:"snapshot"`
}
