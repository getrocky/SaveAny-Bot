// Package cachegc removes cache files that no running task needs any more.
//
// Tasks announce the cache entries they use with Hold; the sweeper never
// touches an entry whose name contains a held key, so deletion decisions rest
// on what this process is doing right now, not on possibly stale database
// status or file times.
package cachegc

import (
	"strings"
	"sync"
)

var (
	holdMu sync.Mutex
	held   = map[string]int{}
)

// Hold marks keys (task IDs, element IDs, temp directory names) as in use.
// A cache entry whose name contains any held key is protected. The returned
// function releases the hold; it is safe to call more than once.
func Hold(keys ...string) func() {
	clean := make([]string, 0, len(keys))
	for _, k := range keys {
		// Very short keys would match unrelated names.
		if len(k) >= 8 {
			clean = append(clean, k)
		}
	}
	holdMu.Lock()
	for _, k := range clean {
		held[k]++
	}
	holdMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			holdMu.Lock()
			defer holdMu.Unlock()
			for _, k := range clean {
				if held[k] <= 1 {
					delete(held, k)
				} else {
					held[k]--
				}
			}
		})
	}
}

// IsHeld reports whether name contains a held key.
func IsHeld(name string) bool {
	holdMu.Lock()
	defer holdMu.Unlock()
	for k := range held {
		if strings.Contains(name, k) {
			return true
		}
	}
	return false
}
