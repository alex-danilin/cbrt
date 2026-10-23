package collect

import (
	"compress/gzip"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	"github.com/alex-danilin/cbrt/internal/analyze"
)

// The parse cache (§3.2.1) keeps each logical source's parsed events in
// <home>/cache/<adapter>/<sha1 of the source key>.json.gz. An entry is valid
// only while the adapter's Version and every member's size and head hash are
// unchanged; any mismatch re-parses the whole source. It is derived data:
// deleting cache/ changes no output.

const (
	cacheVersion = 1
	headBytes    = 64 << 10
)

type cacheMember struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	Head string `json:"head"` // hex sha1 of the first 64 KiB
}

type cacheEntry struct {
	V       int             `json:"v"`
	Key     string          `json:"key"`
	Version string          `json:"version"` // adapter version
	Members []cacheMember   `json:"members"`
	Events  []analyze.Event `json:"events"`
}

// sourceKey identifies a source in the cache, locator included so that the
// same session under two roots does not share an entry.
func sourceKey(src analyze.Source) string {
	return src.AdapterID + "\x00" + src.Store + "\x00" + src.NativeSessionID + "\x00" + src.Locator
}

func (c *collector) cachePath(src analyze.Source) string {
	sum := sha1.Sum([]byte(sourceKey(src)))
	return filepath.Join(c.home, "cache", src.AdapterID, hex.EncodeToString(sum[:])+".json.gz")
}

// fingerprint stats and head-hashes every member.
func fingerprint(src analyze.Source) ([]cacheMember, error) {
	out := make([]cacheMember, 0, len(src.Members))
	for _, m := range src.Members {
		f, err := os.Open(m.Path)
		if err != nil {
			return nil, err
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, err
		}
		h := sha1.New()
		_, err = io.Copy(h, io.LimitReader(f, headBytes))
		f.Close()
		if err != nil {
			return nil, err
		}
		out = append(out, cacheMember{Path: m.Path, Size: st.Size(), Head: hex.EncodeToString(h.Sum(nil))})
	}
	return out, nil
}

func sameMembers(a, b []cacheMember) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sourceEvents returns a source's events, from the cache when it is valid.
// Sources without members (registry-only) are never cached.
func (c *collector) sourceEvents(a analyze.Adapter, src analyze.Source) ([]analyze.Event, error) {
	useCache := !c.noCache && len(src.Members) > 0
	var fp []cacheMember
	if useCache {
		var err error
		if fp, err = fingerprint(src); err != nil {
			useCache = false // let Events report the unreadable member
		}
	}
	path := ""
	if useCache {
		path = c.cachePath(src)
		if evs, ok := readCache(path, sourceKey(src), a.Version(), fp); ok {
			return evs, nil
		}
	}
	var evs []analyze.Event
	if err := a.Events(src, func(e analyze.Event) error {
		evs = append(evs, e)
		return nil
	}); err != nil {
		return nil, err
	}
	if useCache {
		// Best effort: a cache that cannot be written only costs a re-parse.
		_ = writeCache(path, cacheEntry{V: cacheVersion, Key: sourceKey(src), Version: a.Version(), Members: fp, Events: evs})
	}
	return evs, nil
}

func readCache(path, key, version string, fp []cacheMember) ([]analyze.Event, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, false
	}
	defer zr.Close()
	var ce cacheEntry
	if err := json.NewDecoder(zr).Decode(&ce); err != nil {
		return nil, false
	}
	if ce.V != cacheVersion || ce.Key != key || ce.Version != version || !sameMembers(ce.Members, fp) {
		return nil, false
	}
	return ce.Events, true
}

// writeCache writes the entry to a temp file in the same directory and
// renames it into place, so a reader never sees a partial entry.
func writeCache(path string, ce cacheEntry) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	zw := gzip.NewWriter(tmp)
	if err = json.NewEncoder(zw).Encode(ce); err != nil {
		return err
	}
	if err = zw.Close(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
