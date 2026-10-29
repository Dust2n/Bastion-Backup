package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"bastion/internal/chunker"
	"bastion/internal/fsx"
	"bastion/internal/layout"
	"bastion/internal/platform"
	"bastion/internal/proto"
	"bastion/internal/repo"
	"bastion/internal/transfer"
)

func newSnapshotID() string {
	b := make([]byte, 3)
	rand.Read(b)
	return time.Now().UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b)
}

type backupRun struct {
	a       *Agent
	rep     *reporter
	up      *transfer.Uploader
	mu      sync.Mutex // guards errLogs and Volume.Errors
	files   int64
	bytes   int64
	total   int64
	errLogs int
}

func (a *Agent) backup(ctx context.Context, rep *reporter, p proto.BackupParams) proto.JobResult {
	lowPriority(true)
	defer lowPriority(false)

	host, _ := os.Hostname()
	snap := &repo.Snapshot{
		ID: newSnapshotID(), AgentID: a.cfg.AgentID, Hostname: host, OS: runtime.GOOS,
		PlanID: p.PlanID, Kind: p.Kind, Time: time.Now().UTC(), Parent: p.Parent,
	}
	var parent *repo.Snapshot
	if p.Parent != "" {
		if s, err := a.c.Snapshot(ctx, p.Parent); err == nil {
			parent = s
		} else {
			rep.Log("Vorheriges Backup %s nicht lesbar, alle Dateien werden gelesen: %v", p.Parent, err)
		}
	}

	var specs []platform.VolumeSpec
	if p.Kind == "image" {
		m, err := platform.CaptureLayout()
		if err != nil {
			return proto.JobResult{Message: err.Error()}
		}
		snap.Layout = m
		specs = platform.ImageVolumes(m)
		if len(specs) == 0 {
			return proto.JobResult{Message: "keine sicherbaren Volumes gefunden"}
		}
		for _, d := range m.Disks {
			if d.System {
				rep.Log("Systemdatenträger %d: %s, %s, %d Partitionen", d.Number, d.Model, layout.Human(d.Size), len(d.Partitions))
			}
		}
	} else {
		for _, path := range p.Paths {
			path = strings.TrimSpace(path)
			if path == "" {
				continue
			}
			if _, err := os.Stat(path); err != nil {
				rep.Log("Pfad %s: %v", path, err)
				snap.Warnings = append(snap.Warnings, fmt.Sprintf("%s nicht gefunden", path))
				continue
			}
			specs = append(specs, platform.VolumeSpec{Key: path, Source: path})
		}
		if len(specs) == 0 {
			return proto.JobResult{Message: "keine der angegebenen Quellen existiert"}
		}
	}

	r := &backupRun{a: a, rep: rep}
	for _, s := range specs {
		r.total += s.Used
	}
	r.up = transfer.NewUploader(ctx, a.c, transfer.NewLimiter(int64(p.BandwidthKBps)*1024))

	var failed error
	for _, spec := range specs {
		var pv *repo.Volume
		if parent != nil {
			for i := range parent.Volumes {
				if parent.Volumes[i].Key == spec.Key {
					pv = &parent.Volumes[i]
				}
			}
		}
		vol, warn, err := r.volume(ctx, spec, p, pv)
		if warn != "" {
			snap.Warnings = append(snap.Warnings, warn)
		}
		if err != nil {
			failed = fmt.Errorf("%s: %w", spec.Key, err)
			break
		}
		snap.Volumes = append(snap.Volumes, *vol)
	}
	upErr := r.up.Close()
	if failed == nil {
		failed = upErr
	}
	if failed == nil && ctx.Err() != nil {
		failed = ctx.Err()
	}
	if failed != nil {
		return proto.JobResult{Message: failed.Error()}
	}

	var errs int64
	for i := range snap.Volumes {
		errs += snap.Volumes[i].Errors
	}
	newBytes := r.up.NewBytes.Load()
	// distribute transferred bytes for statistics
	if len(snap.Volumes) > 0 {
		snap.Volumes[0].NewBytes = newBytes
	}
	if _, err := a.c.JSON(ctx, "POST", "/api/agent/snapshots", snap, nil); err != nil {
		return proto.JobResult{Message: "Backup konnte nicht abgeschlossen werden: " + err.Error()}
	}
	msg := fmt.Sprintf("%d Dateien, %s gelesen, %s neu übertragen", r.files, layout.Human(r.bytes), layout.Human(newBytes))
	if errs > 0 {
		msg += fmt.Sprintf(", %d Dateien nicht lesbar", errs)
	}
	return proto.JobResult{Success: true, Message: msg, SnapshotID: snap.ID}
}

func (r *backupRun) volume(ctx context.Context, spec platform.VolumeSpec, p proto.BackupParams, parent *repo.Volume) (*repo.Volume, string, error) {
	vol := &repo.Volume{Key: spec.Key, Root: spec.Source, FS: spec.FS, Label: spec.Label, UUID: spec.UUID,
		Size: spec.Size, Used: spec.Used, Snapshot: "none", WinStream: runtime.GOOS == "windows"}
	readRoot := spec.Source
	var warn string
	if p.UseSnapshot || p.Kind == "image" {
		volName, rel := platform.VolumeOf(spec.Source)
		r.rep.Log("Erstelle Snapshot von %s", volName)
		s, err := platform.CreateSnapshot(volName)
		if err != nil {
			return nil, "", err
		}
		defer s.Release()
		vol.Snapshot = s.Kind
		if s.Info != "" {
			r.rep.Log("%s", s.Info)
		}
		if s.Warning != "" {
			warn = s.Warning
			r.rep.Log("Warnung: %s", s.Warning)
		}
		readRoot = platform.JoinSnap(s.Root, rel)
	}

	excludes := append([]string{}, p.Excludes...)
	if p.Kind == "image" {
		if runtime.GOOS == "windows" {
			excludes = append(excludes, fsx.DefaultWindowsExcludes...)
		} else {
			excludes = append(excludes, fsx.DefaultLinuxExcludes...)
		}
	}

	prev := map[string]*repo.Node{}
	if parent != nil {
		err := repo.ReadTree(repo.NewChunkReader(parent.Tree, func(id repo.ID) ([]byte, error) { return r.a.c.GetChunk(ctx, id) }),
			func(n *repo.Node) error {
				if n.Type == repo.TypeFile || n.Type == repo.TypeDir {
					prev[n.Path] = n
				}
				return nil
			})
		if err != nil {
			r.rep.Log("Vorheriger Verzeichnisbaum nicht lesbar, lese alles: %v", err)
			prev = map[string]*repo.Node{}
		} else {
			r.rep.Log("Inkrementell: %d bekannte Einträge aus dem letzten Backup", len(prev))
		}
	}

	pr, pw := io.Pipe()
	treeDone := make(chan error, 1)
	go func() {
		ch := chunker.New(pr)
		for {
			b, err := ch.Next()
			if err == io.EOF {
				treeDone <- nil
				return
			}
			if err != nil {
				pr.CloseWithError(err)
				treeDone <- err
				return
			}
			id, err := r.up.Add(b)
			if err != nil {
				pr.CloseWithError(err)
				treeDone <- err
				return
			}
			vol.Tree = append(vol.Tree, id)
		}
	}()
	tw := repo.NewTreeWriter(pw)

	// Files are read by several workers in parallel (small files are bound
	// by I/O latency), while the tree is still written in walk order.
	type item struct {
		n    *repo.Node
		open func() (io.ReadCloser, error)
		done chan struct{}
		err  error
	}
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan *item, 512)
	ordered := make(chan *item, 4096)
	var wg sync.WaitGroup
	for i := 0; i < readWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch := chunker.New(nil)
			for it := range jobs {
				if wctx.Err() == nil {
					it.err = r.content(ch, it.n, it.open)
				} else {
					it.err = wctx.Err()
				}
				close(it.done)
			}
		}()
	}
	logErr := func(path string, err error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		vol.Errors++
		if r.errLogs < 200 {
			r.errLogs++
			r.rep.Log("Nicht lesbar: %s: %v", path, err)
		}
	}

	r.rep.Log("Sichere %s", spec.Key)
	walkDone := make(chan error, 1)
	go func() {
		defer close(ordered)
		defer close(jobs)
		walkDone <- fsx.Walk(readRoot, fsx.WalkOptions{Excludes: excludes, Error: logErr}, func(e *fsx.Entry) error {
			if wctx.Err() != nil {
				return wctx.Err()
			}
			it := &item{n: e.Node, done: make(chan struct{})}
			needRead := false
			if e.Open != nil {
				if old := prev[it.n.Path]; old != nil && unchanged(old, it.n) {
					it.n.Chunks, it.n.Inline, it.n.StreamSize = old.Chunks, old.Inline, old.StreamSize
					r.up.MarkKnown(it.n.Chunks)
				} else {
					it.open, needRead = e.Open, true
				}
			}
			if !needRead {
				close(it.done)
			}
			select {
			case ordered <- it:
			case <-wctx.Done():
				return wctx.Err()
			}
			if needRead {
				select {
				case jobs <- it:
				case <-wctx.Done():
					// already queued for the writer: it must not wait for it
					it.err = wctx.Err()
					close(it.done)
					return wctx.Err()
				}
			}
			return nil
		})
	}()

	var walkErr error
	lastReport := time.Now()
	for it := range ordered {
		<-it.done
		if walkErr != nil {
			continue // drain after a fatal error
		}
		n := it.n
		if it.err != nil {
			if errors.Is(it.err, context.Canceled) || r.up.Err() != nil {
				walkErr = it.err
				cancel()
				continue
			}
			logErr(n.Path, it.err)
			if n.Type != repo.TypeDir {
				continue // skip the file, keep going
			}
			n.Chunks, n.Inline = nil, nil
		}
		if n.Type == repo.TypeFile {
			r.files++
			r.bytes += n.Size
			vol.Files++
			vol.Bytes += n.Size
		}
		if time.Since(lastReport) > 2*time.Second {
			lastReport = time.Now()
			r.rep.Set(func(p *proto.Progress) {
				p.Files, p.Bytes, p.Total = r.files, r.bytes, r.total
				if r.total > 0 {
					p.Percent = min(99, float64(r.bytes)*100/float64(r.total))
				}
				p.Message = fmt.Sprintf("%s: %s", spec.Key, n.Path)
			})
		}
		if err := tw.Write(n); err != nil {
			walkErr = err
			cancel()
		}
	}
	wg.Wait()
	if err := <-walkDone; err != nil && walkErr == nil {
		walkErr = err
	}
	if err := tw.Flush(); err != nil && walkErr == nil {
		walkErr = err
	}
	pw.Close()
	if err := <-treeDone; err != nil && walkErr == nil {
		walkErr = err
	}
	if walkErr != nil {
		return nil, warn, walkErr
	}
	r.rep.Log("%s: %d Dateien, %s", spec.Key, vol.Files, layout.Human(vol.Bytes))
	return vol, warn, nil
}

func unchanged(old, n *repo.Node) bool {
	return old.Type == n.Type && old.Size == n.Size && old.Mtime == n.Mtime && old.Ctime == n.Ctime &&
		old.Mode == n.Mode && old.UID == n.UID && old.GID == n.GID && (old.Inline != nil || len(old.Chunks) > 0 || old.StreamSize == 0)
}

// readWorkers files are read at the same time.
const readWorkers = 8

func (r *backupRun) content(ch *chunker.Chunker, n *repo.Node, open func() (io.ReadCloser, error)) error {
	rc, err := open()
	if err != nil {
		return err
	}
	defer rc.Close()
	head := make([]byte, fsx.InlineLimit+1)
	k, err := io.ReadFull(rc, head)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		if k > 0 {
			n.Inline = head[:k]
		}
		n.StreamSize = int64(k)
		return nil
	}
	if err != nil {
		return err
	}
	ch.Reset(io.MultiReader(bytes.NewReader(head), rc))
	for {
		b, err := ch.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			n.Chunks = nil
			return err
		}
		id, err := r.up.Add(b)
		if err != nil {
			return err
		}
		n.Chunks = append(n.Chunks, id)
		n.StreamSize += int64(len(b))
	}
}

func (a *Agent) restoreFiles(ctx context.Context, rep *reporter, p proto.RestoreFilesParams) proto.JobResult {
	snap, err := a.c.Snapshot(ctx, p.SnapshotID)
	if err != nil {
		return proto.JobResult{Message: err.Error()}
	}
	var vol *repo.Volume
	for i := range snap.Volumes {
		if snap.Volumes[i].Key == p.Volume {
			vol = &snap.Volumes[i]
		}
	}
	if vol == nil {
		return proto.JobResult{Message: "Volume " + p.Volume + " nicht im Backup"}
	}
	if vol.WinStream != (runtime.GOOS == "windows") {
		return proto.JobResult{Message: "Backup stammt von einem anderen Betriebssystem; Dateien bitte über die Weboberfläche herunterladen"}
	}
	root := vol.Root
	if p.Target != "" {
		root = p.Target
	}
	if p.Format {
		if p.Target != "" || len(p.Paths) > 0 {
			return proto.JobResult{Message: "Formatieren ist nur bei Wiederherstellung des ganzen Volumes am Originalort möglich"}
		}
		rep.Log("Formatiere %s", vol.Key)
		if err := platform.FormatVolume(vol.Key, vol.FS, vol.Label); err != nil {
			return proto.JobResult{Message: err.Error()}
		}
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return proto.JobResult{Message: err.Error()}
	}
	w := fsx.NewWriter(root)
	w.Overwrite = p.Overwrite
	w.DelayLocked = true
	sel := append([]string(nil), p.Paths...)
	sort.Strings(sel)
	rep.Log("Stelle %s nach %s wieder her (%d Auswahl)", vol.Key, root, len(sel))
	st, err := transfer.RestoreVolume(ctx, func(ctx context.Context, id repo.ID) ([]byte, error) { return a.c.GetChunk(ctx, id) },
		*vol, w, sel, func(s transfer.RestoreStats) {
			rep.Set(func(pr *proto.Progress) {
				pr.Files, pr.Bytes, pr.Total = s.Files, s.Bytes, vol.Bytes
				if vol.Bytes > 0 {
					pr.Percent = min(99, float64(s.Bytes)*100/float64(vol.Bytes))
				}
			})
		})
	for _, e := range st.Errors {
		rep.Log("Fehler: %s", e)
	}
	if err != nil {
		return proto.JobResult{Message: err.Error()}
	}
	msg := fmt.Sprintf("%d Dateien (%s) wiederhergestellt", st.Files, layout.Human(st.Bytes))
	if len(st.Errors) > 0 {
		msg += fmt.Sprintf(", %d Fehler", len(st.Errors))
	}
	if w.PendingReboot {
		msg += "; geöffnete Dateien werden beim nächsten Neustart ersetzt"
	}
	return proto.JobResult{Success: len(st.Errors) == 0 || st.Files > 0, Message: msg}
}
