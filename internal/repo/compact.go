package repo

import (
	"os"
	"path/filepath"
	"time"
)

// CompactStats is the result of Compact.
type CompactStats struct {
	Chunks   int64 // chunks in the store afterwards
	Bytes    int64 // space on disk afterwards
	Freed    int64 // space given back
	Migrated int64 // chunks moved from single files into packs
	Repacked int64 // chunks moved out of packs with too much dead space
}

// repackDead is the share of unused space from which a pack is rewritten.
const repackDead = 0.3

// Compact removes what no backup needs any more. live holds every chunk
// that is still referenced; nothing younger than cutoff is touched, it may
// belong to a backup that is being written.
//
//   - Chunk files of the old format: unreferenced ones are deleted, the
//     others move into packs.
//   - Packs without a referenced chunk are deleted.
//   - Packs with more than 30 % unused space are rewritten: the referenced
//     chunks move to a new pack, the old one is deleted.
//
// A chunk is only deleted from its old place after its new place is on
// disk (Flush), so a crash at any point loses nothing; at worst a chunk
// exists twice until the next run.
func (s *Store) Compact(live map[ID]struct{}, cutoff time.Time) (CompactStats, error) {
	var st CompactStats
	// what is being written now is finished first; it is younger than cutoff
	if err := s.Seal(); err != nil {
		return st, err
	}
	if err := s.compactLoose(live, cutoff, &st); err != nil {
		return st, err
	}
	if err := s.compactPacks(live, cutoff, &st); err != nil {
		return st, err
	}
	if err := s.Seal(); err != nil {
		return st, err
	}
	st.Chunks, st.Bytes = s.Stats()
	return st, nil
}

func (s *Store) compactLoose(live map[ID]struct{}, cutoff time.Time, st *CompactStats) error {
	s.mu.RLock()
	loose := s.hasLoose
	s.mu.RUnlock()
	if !loose {
		return nil
	}
	// files whose content has been appended to a pack but not yet synced
	var pending []string
	var pendingBytes int64
	commit := func() error {
		if len(pending) == 0 {
			return nil
		}
		if err := s.Flush(); err != nil {
			return err
		}
		for _, p := range pending {
			os.Remove(p)
		}
		pending, pendingBytes = pending[:0], 0
		return nil
	}
	err := s.walkLoose(func(c ChunkInfo, p string) error {
		s.mu.RLock()
		_, packed := s.idx[c.ID]
		s.mu.RUnlock()
		_, isLive := live[c.ID]
		switch {
		case packed || c.Size < minChunkFile:
			// already in a pack, or a torn write that never counted
			if os.Remove(p) == nil && !packed {
				st.Freed += c.Size
			}
		case !isLive && c.ModTime.Before(cutoff):
			if os.Remove(p) == nil {
				st.Freed += c.Size
			}
		default:
			b, err := os.ReadFile(p)
			if err != nil {
				return nil // unreadable: leave it, the integrity check deals with it
			}
			if err := s.appendBlob(c.ID, b, false); err != nil {
				return err
			}
			st.Migrated++
			pending = append(pending, p)
			pendingBytes += c.Size
			if pendingBytes >= s.PackSize {
				return commit()
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := commit(); err != nil {
		return err
	}
	// drop the emptied directories, so Has no longer has to look there
	root := filepath.Join(s.dir, "chunks")
	if subs, err := os.ReadDir(root); err == nil {
		for _, a := range subs {
			if !a.IsDir() {
				continue
			}
			if inner, err := os.ReadDir(filepath.Join(root, a.Name())); err == nil {
				for _, b := range inner {
					os.Remove(filepath.Join(root, a.Name(), b.Name())) // fails unless empty
				}
			}
			os.Remove(filepath.Join(root, a.Name()))
		}
	}
	s.mu.Lock()
	s.hasLoose = dirHasEntries(root)
	s.mu.Unlock()
	return nil
}

func (s *Store) compactPacks(live map[ID]struct{}, cutoff time.Time, st *CompactStats) error {
	// space each pack still needs, and which of its chunks are referenced
	type use struct {
		liveBytes int64
		ids       []ID // referenced
		dead      []ID // indexed, but no longer referenced
	}
	s.mu.RLock()
	used := make(map[packID]*use, len(s.packs))
	sizes := make(map[packID]int64, len(s.packs))
	for p, size := range s.packs {
		used[p] = &use{}
		sizes[p] = size
	}
	for id, l := range s.idx {
		u, ok := used[l.pack]
		if !ok {
			continue // the pack being written
		}
		if _, isLive := live[id]; isLive {
			u.liveBytes += recHeader + int64(l.n)
			u.ids = append(u.ids, id)
		} else {
			u.dead = append(u.dead, id)
		}
	}
	s.mu.RUnlock()

	for p, u := range used {
		size := sizes[p]
		fi, err := os.Stat(s.packPath(p))
		if err != nil || !fi.ModTime().Before(cutoff) {
			continue
		}
		dead := size - int64(len(packMagic)) - u.liveBytes
		if len(u.ids) > 0 && float64(dead) < repackDead*float64(size) {
			continue
		}
		// move what is still needed, make it durable, then drop the pack
		for _, id := range u.ids {
			blob, err := s.readBlob(id)
			if err != nil {
				return err
			}
			if err := s.appendBlob(id, blob, true); err != nil {
				return err
			}
			st.Repacked++
		}
		if len(u.ids) > 0 {
			if err := s.Flush(); err != nil {
				return err
			}
		}
		s.mu.Lock()
		for _, id := range u.dead {
			if l, ok := s.idx[id]; ok && l.pack == p {
				delete(s.idx, id)
			}
		}
		delete(s.packs, p)
		s.mu.Unlock()
		// index first: a pack without index would be read again at the next start
		os.Remove(s.idxPath(p))
		// On Windows a file can not be deleted while a restore is reading
		// from it. Then it stays for now: without its index it is read
		// again at the next start and removed by a later run.
		var rerr error
		for try := 0; try < 5; try++ {
			if rerr = os.Remove(s.packPath(p)); rerr == nil || os.IsNotExist(rerr) {
				rerr = nil
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if rerr == nil {
			st.Freed += size - u.liveBytes
		}
	}
	return nil
}
