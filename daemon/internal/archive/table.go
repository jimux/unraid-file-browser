package archive

import (
	"path"
	"sort"
	"strings"
	"sync"

	"unraid-filebrowser/internal/types"
)

// entryRecord is one flat entry of an archive listing, driver-agnostic.
//
// Drivers fill raw (the name exactly as stored) and leave path empty; the
// table fills path and blanks raw whenever the two are identical, so a clean
// name costs one string header, not two. Use storedName() for extraction.
type entryRecord struct {
	path    string // cleaned, "/"-separated, no leading slash; "" = archive root
	raw     string // stored name when it differs from path (else "")
	size    int64  // -1 unknown
	mtime   int64  // unix seconds, 0 unknown
	isDir   bool
	symlink string // target when the entry is a symlink
}

// storedName is the name to hand to the driver when extracting this entry.
func (e entryRecord) storedName() string {
	if e.raw != "" {
		return e.raw
	}
	return e.path
}

// errTooManyEntries is returned by every lister and by newTable once an
// archive exceeds Options.MaxEntries.
func errTooManyEntries() error {
	return types.Errf(types.ErrArchive, "archive has too many entries")
}

// table indexes the flat entry list of one archive so that List(subdir) and
// entry lookup work with implicit intermediate directories (tar/zip/7z
// listings commonly omit directory headers for "a" and "a/b" of "a/b/c.txt").
//
// Layout is deliberately compact because tables are pinned by the LRU: one
// record slice (index 0 = root), one path→index map, and per-directory
// sorted child index slices.
type table struct {
	recs     []entryRecord    // all nodes; recs[0] is the archive root
	index    map[string]int32 // cleaned path -> index into recs
	children map[int32][]int32
	bytes    int64 // estimated resident footprint, for the cache budget
}

// tableNodeOverhead approximates the fixed per-node cost (record struct,
// map bucket share, child index) on top of the string payloads.
const tableNodeOverhead = 128

// newTable builds a table from raw driver records. Records whose stored name
// is absolute or escapes upward (zip-slip data) are dropped. maxNodes > 0
// caps the number of nodes (entries plus synthesized directories, root
// excluded); exceeding it returns ErrArchive.
func newTable(recs []entryRecord, maxNodes int) (*table, error) {
	t := &table{
		recs:     make([]entryRecord, 1, len(recs)+1),
		index:    make(map[string]int32, len(recs)+1),
		children: make(map[int32][]int32),
	}
	t.recs[0] = entryRecord{path: "", isDir: true}
	t.index[""] = 0
	for _, rec := range recs {
		p, ok := cleanEntryName(rec.raw)
		if !ok || p == "" {
			continue
		}
		rec.path = p
		if rec.raw == p {
			rec.raw = ""
		}
		i, existed, err := t.node(p, maxNodes)
		if err != nil {
			return nil, err
		}
		if !existed {
			t.recs[i] = rec
			t.bytes += int64(len(rec.raw) + len(rec.symlink))
			continue
		}
		cur := &t.recs[i]
		switch {
		case rec.isDir:
			// Explicit directory replaces an implicit one (or a same-named
			// file: directories win, only possible in hostile archives).
			t.bytes += int64(len(rec.raw) + len(rec.symlink) - len(cur.raw) - len(cur.symlink))
			*cur = rec
		case cur.isDir:
			// File under a directory's name: the directory wins.
		default:
			// Duplicate file name: first occurrence wins.
		}
	}
	for _, kids := range t.children {
		sort.Slice(kids, func(a, b int) bool {
			return baseOf(t.recs[kids[a]].path) < baseOf(t.recs[kids[b]].path)
		})
	}
	return t, nil
}

// node returns the index of path p, creating it (as an implicit directory)
// and every missing ancestor when absent. existed reports whether p was
// already present.
func (t *table) node(p string, maxNodes int) (idx int32, existed bool, err error) {
	if i, ok := t.index[p]; ok {
		return i, true, nil
	}
	parent, _, perr := t.node(parentOf(p), maxNodes)
	if perr != nil {
		return 0, false, perr
	}
	if pr := &t.recs[parent]; !pr.isDir {
		// A file name that also prefixes deeper entries: directories win.
		t.bytes -= int64(len(pr.raw) + len(pr.symlink))
		*pr = entryRecord{path: pr.path, isDir: true}
	}
	if maxNodes > 0 && len(t.recs) > maxNodes {
		return 0, false, errTooManyEntries()
	}
	idx = int32(len(t.recs))
	t.recs = append(t.recs, entryRecord{path: p, isDir: true})
	t.index[p] = idx
	t.children[parent] = append(t.children[parent], idx)
	t.bytes += tableNodeOverhead + int64(len(p))
	return idx, false, nil
}

// lookup finds the record at a cleaned entry path ("" = root).
func (t *table) lookup(p string) (entryRecord, bool) {
	i, ok := t.index[p]
	if !ok {
		return entryRecord{}, false
	}
	return t.recs[i], true
}

// listDir returns the direct children of directory p ("" = root), sorted by
// name. ok is false when p is not a directory of this archive.
func (t *table) listDir(p string) ([]entryRecord, bool) {
	i, ok := t.index[p]
	if !ok || !t.recs[i].isDir {
		return nil, false
	}
	kids := t.children[i]
	out := make([]entryRecord, 0, len(kids))
	for _, k := range kids {
		out = append(out, t.recs[k])
	}
	return out, true
}

// count is the number of nodes excluding the root.
func (t *table) count() int { return len(t.recs) - 1 }

func joinEntry(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// cleanEntryName normalizes an archive-stored entry name. Names are DATA:
// absolute names and names escaping upward via ".." are rejected (ok=false).
// Trailing slashes (directory markers) are dropped.
func cleanEntryName(name string) (string, bool) {
	if name == "" || strings.ContainsRune(name, 0) {
		return "", false
	}
	if strings.HasPrefix(name, "/") {
		return "", false
	}
	c := path.Clean(name)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", false
	}
	return c, true
}

// tableCache is a small mutex-guarded LRU of parsed entry tables, keyed by
// archive identity (real path + mtime + size, plus the nested entry chain).
// It is bounded both by slot count and by the summed estimated table size;
// the most recently inserted table is always retained so one oversized
// listing still serves its own request.
type tableCache struct {
	mu       sync.Mutex
	cap      int
	maxBytes int64
	bytes    int64
	keys     []string // LRU order: oldest first
	vals     map[string]*table
}

func newTableCache(capacity int, maxBytes int64) *tableCache {
	return &tableCache{cap: capacity, maxBytes: maxBytes, vals: make(map[string]*table)}
}

func (c *tableCache) get(key string) (*table, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.vals[key]
	if ok {
		c.touchLocked(key)
	}
	return t, ok
}

func (c *tableCache) put(key string, t *table) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.vals[key]; ok {
		c.bytes += t.bytes - old.bytes
		c.vals[key] = t
		c.touchLocked(key)
	} else {
		c.vals[key] = t
		c.keys = append(c.keys, key)
		c.bytes += t.bytes
	}
	for len(c.keys) > 1 && (len(c.keys) > c.cap || c.bytes > c.maxBytes) {
		evict := c.keys[0]
		c.keys = c.keys[1:]
		c.bytes -= c.vals[evict].bytes
		delete(c.vals, evict)
	}
}

func (c *tableCache) touchLocked(key string) {
	for i, k := range c.keys {
		if k == key {
			c.keys = append(c.keys[:i], c.keys[i+1:]...)
			c.keys = append(c.keys, key)
			return
		}
	}
}
