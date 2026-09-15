package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/brown-enterprises/be-code/internal/repomap"
)

// Event is one tool call and its result, as dispatch saw them.
type Event struct {
	Tool    string
	Args    map[string]any
	Content string
	IsError bool
}

const (
	footerAlreadyRead = "already read at turn %d (unchanged); outline and notes are in your context"
	footerCached      = "(cached; files unchanged)"
)

var numberedLine = regexp.MustCompile(`(?m)^\s*(\d+)\t`)
var hitLine = regexp.MustCompile(`^([^\s:][^:]*):(\d+):(.*)$`)

func argStr(args map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := args[k]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func relPath(p string) string { return filepath.ToSlash(filepath.Clean(p)) }

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// fileState reads a workspace file and returns its hash, size and mtime;
// ok is false when it cannot be read.
func (s *Store) fileState(rel string) (data []byte, hash string, size, mtime int64, ok bool) {
	abs := filepath.Join(s.root, filepath.FromSlash(rel))
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		return nil, "", 0, 0, false
	}
	data, err = os.ReadFile(abs)
	if err != nil {
		return nil, "", 0, 0, false
	}
	return data, hashBytes(data), info.Size(), info.ModTime().UnixNano(), true
}

// Observe records what a tool result revealed and returns a footer for the
// model when the read was redundant. Never errors; never blocks on I/O
// beyond one stat and one read of a file the model just read.
func (s *Store) Observe(ev Event) string {
	if ev.IsError {
		return ""
	}
	switch ev.Tool {
	case "read_file":
		return s.observeRead(ev)
	case "write_file", "edit_file":
		s.observeWrite(ev)
	case "search", "lookup", "history":
		s.observeLookup(ev)
	}
	return ""
}

// seenRange works out which lines a read_file result covered.
func seenRange(ev Event, total int) Range {
	nums := numberedLine.FindAllStringSubmatch(ev.Content, -1)
	if len(nums) == 0 {
		return Range{1, total}
	}
	from, _ := strconv.Atoi(nums[0][1])
	to, _ := strconv.Atoi(nums[len(nums)-1][1])
	if from < 1 {
		from = 1
	}
	if to < from {
		to = from
	}
	return Range{from, to}
}

func covered(ranges []Range, r Range) bool {
	// Ranges are merged on insert, so one of them must contain r entirely.
	for _, have := range ranges {
		if have.From <= r.From && have.To >= r.To {
			return true
		}
	}
	return false
}

func mergeRange(ranges []Range, r Range) []Range {
	ranges = append(ranges, r)
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].From < ranges[j].From })
	out := ranges[:1]
	for _, n := range ranges[1:] {
		last := &out[len(out)-1]
		if n.From <= last.To+1 {
			if n.To > last.To {
				last.To = n.To
			}
			continue
		}
		out = append(out, n)
	}
	return out
}

func (s *Store) observeRead(ev Event) string {
	rel := relPath(argStr(ev.Args, "path", "file", "filename"))
	if rel == "" || rel == "." {
		return ""
	}
	data, hash, size, mtime, ok := s.fileState(rel)
	if !ok {
		return ""
	}
	total := strings.Count(string(data), "\n")
	if len(data) > 0 && data[len(data)-1] != '\n' {
		total++
	}
	r := seenRange(ev, total)
	s.mu.Lock()
	defer s.mu.Unlock()
	d, exists := s.digests[rel]
	if exists && d.Hash == hash && covered(d.Ranges, r) {
		turn := d.Turn
		d.Turn = s.turn
		s.touchLocked(d)
		s.dirty = true
		return fmt.Sprintf(footerAlreadyRead, turn)
	}
	if !exists || d.Hash != hash {
		nd := &Digest{Path: rel}
		if exists {
			nd.Note = d.Note
			nd.Edited = d.Edited
		}
		nd.Hash, nd.Size, nd.ModTime = hash, size, mtime
		nd.Outline = repomap.Outline(rel, data)
		nd.Ranges = []Range{r}
		nd.Turn = s.turn
		s.putDigest(nd)
		return ""
	}
	d.Ranges = mergeRange(d.Ranges, r)
	d.Turn = s.turn
	s.touchLocked(d)
	s.dirty = true
	return ""
}

func (s *Store) observeWrite(ev Event) {
	rel := relPath(argStr(ev.Args, "path", "file", "filename"))
	if rel == "" || rel == "." {
		return
	}
	data, hash, size, mtime, ok := s.fileState(rel)
	if !ok {
		return
	}
	total := strings.Count(string(data), "\n")
	if len(data) > 0 && data[len(data)-1] != '\n' {
		total++
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, exists := s.digests[rel]
	if !exists {
		d = &Digest{Path: rel}
	}
	d.Hash, d.Size, d.ModTime = hash, size, mtime
	d.Outline = repomap.Outline(rel, data)
	d.Ranges = []Range{{1, total}}
	d.Edited = true
	d.Turn = s.turn
	s.putDigest(d)
}

// LookupKey canonicalises a lookup so argument order never misses the cache.
func LookupKey(tool string, args map[string]any) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(tool)
	b.WriteByte(' ')
	for _, k := range keys {
		v, _ := json.Marshal(args[k])
		b.WriteString(k)
		b.WriteByte('=')
		b.Write(v)
		b.WriteByte(';')
	}
	return b.String()
}

// parseHits reads file:line:text lines out of a search or lookup result;
// context lines (file-line-text, git grep's context form) are deliberately
// not parsed, since a bare "-" separator is indistinguishable from one
// inside a filename.
func parseHits(content string) []Hit {
	var hits []Hit
	for _, line := range strings.Split(content, "\n") {
		m := hitLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[2])
		text := strings.TrimSpace(m[3])
		if len(text) > 120 {
			text = text[:120]
		}
		hits = append(hits, Hit{File: relPath(m[1]), Line: n, Text: text})
		if len(hits) >= 50 {
			break
		}
	}
	return hits
}

func (s *Store) observeLookup(ev Event) {
	key := LookupKey(ev.Tool, ev.Args)
	hits := parseHits(ev.Content)
	hashes := map[string]string{}
	for _, h := range hits {
		if _, ok := hashes[h.File]; ok {
			continue
		}
		if _, hash, _, _, ok := s.fileState(h.File); ok {
			hashes[h.File] = hash
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.lookups {
		if s.lookups[i].Query == key {
			s.lookups = append(s.lookups[:i], s.lookups[i+1:]...)
			break
		}
	}
	s.lookups = append(s.lookups, Lookup{Tool: ev.Tool, Query: key, Hits: hits, Hashes: hashes, Turn: s.turn})
	if len(s.lookups) > maxLookups {
		s.lookups = s.lookups[len(s.lookups)-maxLookups:]
	}
	if len(hits) > 0 {
		// A zero-hit result has no file to invalidate it against, so
		// caching it would serve "no matches" forever, even after the
		// model creates a file that would now match. Never cache it, and
		// drop any earlier (non-empty) cache entry for the same key.
		s.cache(key, ev.Content)
	} else {
		delete(s.cached, key)
	}
	s.dirty = true
}

// The cached contents live in memory only (a session's worth of lookups is
// small); the hits and hashes are what persist.
func (s *Store) cache(key, content string) {
	if s.cached == nil {
		s.cached = map[string]string{}
	}
	s.cached[key] = content
}

// Cached answers a repeated lookup whose hit files are unchanged.
func (s *Store) Cached(tool string, args map[string]any) (string, bool) {
	key := LookupKey(tool, args)
	s.mu.Lock()
	var lk Lookup
	found := false
	for i := range s.lookups {
		if s.lookups[i].Query == key {
			lk = s.lookups[i] // value copy: observeLookup may replace this slot
			found = true
		}
	}
	content, have := s.cached[key]
	s.mu.Unlock()
	if !found || !have {
		return "", false
	}
	for file, hash := range lk.Hashes {
		if _, now, _, _, ok := s.fileState(file); !ok || now != hash {
			return "", false
		}
	}
	return strings.TrimRight(content, "\n") + "\n" + footerCached, true
}

// HasDigest reports the union of ranges seen for a path.
func (s *Store) HasDigest(path string) (Range, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.digests[relPath(path)]
	if !ok || len(d.Ranges) == 0 {
		return Range{}, false
	}
	return Range{d.Ranges[0].From, d.Ranges[len(d.Ranges)-1].To}, true
}
