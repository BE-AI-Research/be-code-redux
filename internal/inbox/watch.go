package inbox

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Watch polls dir and calls onNew for every message file that appears after
// the watch began, in name order (names begin with the send time). A poll
// rather than inotify: it costs one ReadDir of the root and of each user
// directory whose mtime moved, once a second, and needs no dependency an
// offline install might lack. Files present when the watch starts are the
// inbox's history, not news, and are never delivered.
func Watch(ctx context.Context, dir string, every time.Duration, onNew func(m Message)) {
	if every <= 0 {
		every = time.Second
	}
	seen := map[string]bool{} // "<user>/<file>"
	mtimes := map[string]time.Time{}
	scan := func(deliver bool) {
		users, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		var fresh []string
		for _, u := range users {
			if !u.IsDir() {
				continue
			}
			udir := filepath.Join(dir, u.Name())
			info, err := os.Stat(udir)
			if err != nil {
				continue
			}
			if deliver && info.ModTime().Equal(mtimes[u.Name()]) {
				continue // nothing written here since the last poll
			}
			mtimes[u.Name()] = info.ModTime()
			files, err := os.ReadDir(udir)
			if err != nil {
				continue
			}
			for _, f := range files {
				name := f.Name()
				if f.IsDir() || !strings.HasSuffix(name, ".json") || name == "read.json" || strings.HasPrefix(name, ".tmp") {
					continue
				}
				key := u.Name() + "/" + name
				if seen[key] {
					continue
				}
				seen[key] = true
				if deliver {
					fresh = append(fresh, key)
				}
			}
		}
		sort.Strings(fresh)
		for _, key := range fresh {
			b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(key)))
			if err != nil {
				continue
			}
			var m Message
			if json.Unmarshal(b, &m) != nil || m.From == "" {
				continue
			}
			onNew(m)
		}
	}
	scan(false) // baseline: everything already there is history
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			scan(true)
		}
	}
}
