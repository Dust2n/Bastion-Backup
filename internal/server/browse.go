package server

import (
	"archive/zip"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"bastion/internal/fsx"
	"bastion/internal/repo"
)

// The backup browser works on a compact index per snapshot (names, sizes,
// times and the directory structure), stored in data/cache/<id>.idx. It is
// built in the background after every backup, so opening even large image
// backups is instant. File contents are always read from the real tree.

type idxNode struct {
	Name     string
	Type     string
	Size     int64
	Mtime    int64
	Parent   int32
	Children []int32 // sorted by lower-case name
}

type volIndex struct {
	Nodes []idxNode // [0] is the root
}

type snapIndex struct {
	Vols map[string]*volIndex
}

type indexCache struct {
	mu       sync.Mutex
	order    []string
	m        map[string]*snapIndex
	building map[string]*sync.WaitGroup
}

func newIndexCache() *indexCache {
	return &indexCache{m: map[string]*snapIndex{}, building: map[string]*sync.WaitGroup{}}
}

// treeCache is kept as the name of the field in Server.
type treeCache = indexCache

func newTreeCache(int) *treeCache { return newIndexCache() }

func (s *Server) indexPath(id string) string { return s.path("cache", id+".idx") }

// buildIndex reads all volume trees of a snapshot and writes the index.
func (s *Server) buildIndex(id string) (*snapIndex, error) {
	c := s.trees
	c.mu.Lock()
	if wg, ok := c.building[id]; ok {
		c.mu.Unlock()
		wg.Wait()
		return s.loadIndex(id)
	}
	wg := &sync.WaitGroup{}
	wg.Add(1)
	c.building[id] = wg
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.building, id)
		c.mu.Unlock()
		wg.Done()
	}()

	snap, err := s.loadSnapshot(id)
	if err != nil {
		return nil, err
	}
	store, err := s.snapStore(id)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	idx := &snapIndex{Vols: map[string]*volIndex{}}
	for _, v := range snap.Volumes {
		vi := &volIndex{}
		dirs := map[string]int32{}
		err := repo.ReadTree(repo.NewChunkReader(v.Tree, store.Get), func(n *repo.Node) error {
			i := int32(len(vi.Nodes))
			size := n.Size
			vi.Nodes = append(vi.Nodes, idxNode{Name: n.Name(), Type: n.Type, Size: size, Mtime: n.Mtime / 1e9, Parent: -1})
			if n.Path != "" {
				if p, ok := dirs[n.Dir()]; ok {
					vi.Nodes[i].Parent = p
					vi.Nodes[p].Children = append(vi.Nodes[p].Children, i)
				}
			}
			if n.Type == repo.TypeDir {
				dirs[n.Path] = i
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", v.Key, err)
		}
		for i := range vi.Nodes {
			ch := vi.Nodes[i].Children
			sort.Slice(ch, func(a, b int) bool {
				return strings.ToLower(vi.Nodes[ch[a]].Name) < strings.ToLower(vi.Nodes[ch[b]].Name)
			})
		}
		idx.Vols[v.Key] = vi
	}
	tmp := s.indexPath(id) + ".tmp"
	f, err := os.Create(tmp)
	if err == nil {
		err = gob.NewEncoder(f).Encode(idx)
		f.Close()
		if err == nil {
			err = os.Rename(tmp, s.indexPath(id))
		}
	}
	if err != nil {
		s.log.Printf("Index für %s nicht gespeichert: %v", id, err)
	} else {
		s.log.Printf("Index für Backup %s erstellt (%s)", id, time.Since(start).Round(time.Millisecond))
	}
	s.putIndex(id, idx)
	return idx, nil
}

func (s *Server) putIndex(id string, idx *snapIndex) {
	c := s.trees
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[id]; !ok {
		c.order = append(c.order, id)
	}
	c.m[id] = idx
	for len(c.order) > 4 {
		delete(c.m, c.order[0])
		c.order = c.order[1:]
	}
}

func (s *Server) loadIndex(id string) (*snapIndex, error) {
	c := s.trees
	c.mu.Lock()
	if idx, ok := c.m[id]; ok {
		c.mu.Unlock()
		return idx, nil
	}
	c.mu.Unlock()
	f, err := os.Open(s.indexPath(id))
	if err != nil {
		return s.buildIndex(id)
	}
	defer f.Close()
	var idx snapIndex
	if err := gob.NewDecoder(f).Decode(&idx); err != nil {
		return s.buildIndex(id)
	}
	s.putIndex(id, &idx)
	return &idx, nil
}

// indexMissing builds the index of every snapshot that has none (startup).
func (s *Server) indexMissing() {
	rows, err := s.db.Query(`SELECT id FROM snapshots WHERE deleted=0 ORDER BY time DESC`)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if _, err := os.Stat(s.indexPath(id)); err != nil {
			s.buildIndex(id)
		}
	}
}

// lookup finds the node of a "/"-separated path.
func (vi *volIndex) lookup(p string) (int32, bool) {
	cur := int32(0)
	if len(vi.Nodes) == 0 {
		return 0, false
	}
	for _, part := range strings.Split(strings.Trim(p, "/"), "/") {
		if part == "" {
			continue
		}
		ch := vi.Nodes[cur].Children
		lp := strings.ToLower(part)
		k := sort.Search(len(ch), func(i int) bool { return strings.ToLower(vi.Nodes[ch[i]].Name) >= lp })
		found := false
		for ; k < len(ch) && strings.ToLower(vi.Nodes[ch[k]].Name) == lp; k++ {
			if vi.Nodes[ch[k]].Name == part {
				cur, found = ch[k], true
				break
			}
		}
		if !found {
			return 0, false
		}
	}
	return cur, true
}

func (vi *volIndex) path(i int32) string {
	var parts []string
	for i > 0 {
		parts = append(parts, vi.Nodes[i].Name)
		i = vi.Nodes[i].Parent
		if i < 0 {
			break
		}
	}
	for a, b := 0, len(parts)-1; a < b; a, b = a+1, b-1 {
		parts[a], parts[b] = parts[b], parts[a]
	}
	return strings.Join(parts, "/")
}

type lsEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Type  string `json:"type"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"`
	Items int    `json:"items,omitempty"` // entries of a directory
}

func (s *Server) volIndexFor(r *http.Request) (*volIndex, error) {
	idx, err := s.loadIndex(r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	vi, ok := idx.Vols[r.URL.Query().Get("vol")]
	if !ok {
		return nil, errors.New("Volume nicht im Backup")
	}
	return vi, nil
}

func (s *Server) entry(vi *volIndex, i int32, p string) lsEntry {
	n := vi.Nodes[i]
	return lsEntry{Name: n.Name, Path: p, Type: n.Type, Size: n.Size, Mtime: n.Mtime, Items: len(n.Children)}
}

// handleLs lists a directory page by page (directories first).
func (s *Server) handleLs(w http.ResponseWriter, r *http.Request, u *user) {
	vi, err := s.volIndexFor(r)
	if err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	dir := strings.Trim(r.URL.Query().Get("path"), "/")
	di, ok := vi.lookup(dir)
	if !ok {
		httpErr(w, 404, "Ordner nicht gefunden")
		return
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	ch := append([]int32(nil), vi.Nodes[di].Children...)
	sort.SliceStable(ch, func(a, b int) bool {
		return (vi.Nodes[ch[a]].Type == repo.TypeDir) && (vi.Nodes[ch[b]].Type != repo.TypeDir)
	})
	out := []lsEntry{}
	for k := offset; k < len(ch) && len(out) < limit; k++ {
		p := vi.Nodes[ch[k]].Name
		if dir != "" {
			p = dir + "/" + p
		}
		out = append(out, s.entry(vi, ch[k], p))
	}
	writeJSON(w, map[string]any{"entries": out, "total": len(ch), "offset": offset})
}

// handleSearch finds names containing q (or matching a * / ? pattern).
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request, u *user) {
	vi, err := s.volIndexFor(r)
	if err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if len(q) < 2 {
		httpErr(w, 400, "Mindestens 2 Zeichen eingeben")
		return
	}
	glob := strings.ContainsAny(q, "*?")
	out := []lsEntry{}
	total := 0
	for i := 1; i < len(vi.Nodes); i++ {
		name := strings.ToLower(vi.Nodes[i].Name)
		var hit bool
		if glob {
			hit, _ = path.Match(q, name)
		} else {
			hit = strings.Contains(name, q)
		}
		if !hit {
			continue
		}
		total++
		if len(out) < 500 {
			out = append(out, s.entry(vi, int32(i), vi.path(int32(i))))
		}
	}
	writeJSON(w, map[string]any{"entries": out, "total": total})
}

// findNodes streams the real tree and collects the node at p and, for
// directories, everything below it (plus hard link targets).
func (s *Server) findNodes(snap *repo.Snapshot, store *repo.Store, vol, p string) (*repo.Volume, []*repo.Node, map[string]*repo.Node, error) {
	var v *repo.Volume
	for i := range snap.Volumes {
		if snap.Volumes[i].Key == vol {
			v = &snap.Volumes[i]
		}
	}
	if v == nil {
		return nil, nil, nil, errors.New("Volume nicht im Backup")
	}
	var nodes []*repo.Node
	files := map[string]*repo.Node{}
	err := repo.ReadTree(repo.NewChunkReader(v.Tree, store.Get), func(n *repo.Node) error {
		if n.Type == repo.TypeFile {
			files[n.Path] = n
		}
		if p == "" || n.Path == p || strings.HasPrefix(n.Path, p+"/") {
			nodes = append(nodes, n)
		}
		return nil
	})
	return v, nodes, files, err
}

// content writes the plain file content of n (unwrapping Windows backup streams).
func (s *Server) content(store *repo.Store, v *repo.Volume, files map[string]*repo.Node, n *repo.Node, w io.Writer) error {
	if n.Type == repo.TypeHardlink {
		if tgt := files[n.Link]; tgt != nil {
			n = tgt
		}
	}
	var r io.Reader
	if n.Inline != nil {
		r = strings.NewReader(string(n.Inline))
	} else {
		r = repo.NewChunkReader(n.Chunks, store.Get)
	}
	if v.WinStream {
		return fsx.ExtractData(r, w)
	}
	_, err := io.Copy(w, r)
	return err
}

func (s *Server) handleDownloadFile(w http.ResponseWriter, r *http.Request, u *user) {
	snap, err := s.loadSnapshot(r.PathValue("id"))
	if err != nil {
		httpErr(w, 404, "%v", err)
		return
	}
	store, err := s.snapStore(snap.ID)
	if err != nil {
		httpErr(w, 503, "%v", err)
		return
	}
	p := strings.Trim(r.URL.Query().Get("path"), "/")
	v, nodes, files, err := s.findNodes(snap, store, r.URL.Query().Get("vol"), p)
	if err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	if len(nodes) == 0 {
		httpErr(w, 404, "nicht gefunden")
		return
	}
	s.audit(u.Name, "download", fmt.Sprintf("%s %s %s", snap.ID, v.Key, p))
	first := nodes[0]
	name := first.Name()
	if name == "" {
		name = "volume"
	}
	if first.Type == repo.TypeFile || first.Type == repo.TypeHardlink {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename*=UTF-8''%s`, urlEscape(name)))
		if first.Type == repo.TypeFile {
			w.Header().Set("Content-Length", fmt.Sprint(first.Size))
		}
		s.content(store, v, files, first, w)
		return
	}
	// directories as ZIP
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename*=UTF-8''%s.zip`, urlEscape(name)))
	zw := zip.NewWriter(w)
	defer zw.Close()
	for _, c := range nodes {
		if c.Type != repo.TypeFile && c.Type != repo.TypeHardlink {
			continue
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(c.Path, p), "/")
		if p != "" {
			rel = path.Join(name, rel)
		}
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: rel, Method: zip.Deflate, Modified: time.Unix(0, c.Mtime)})
		if err != nil {
			return
		}
		if err := s.content(store, v, files, c, fw); err != nil {
			s.log.Printf("ZIP-Download: %v", err)
			return
		}
	}
}

func urlEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.IndexByte("-._~", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
