package batchtfile

import (
	"slices"
	"testing"
)

func TestCacheKeysListsElementIDs(t *testing.T) {
	task := &Task{elems: []TaskElement{{ID: "aaaaaaaaaaaaaaaaaaaa"}, {ID: "bbbbbbbbbbbbbbbbbbbb"}}}
	if got := task.CacheKeys(); !slices.Equal(got, []string{"aaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbb"}) {
		t.Fatalf("CacheKeys() = %v", got)
	}
}
