package handlers

import (
	"testing"
	"time"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"
	"github.com/krau/SaveAny-Bot/pkg/tfile"
)

type fakeGroupFile struct{ msg *tg.Message }

func (f fakeGroupFile) Location() tg.InputFileLocationClass { return nil }
func (f fakeGroupFile) Dler() downloader.Client             { return nil }
func (f fakeGroupFile) Size() int64                         { return 0 }
func (f fakeGroupFile) Name() string                        { return "" }
func (f fakeGroupFile) SetName(string)                      {}
func (f fakeGroupFile) Message() *tg.Message                { return f.msg }

func groupFile(id int) tfile.TGFileMessage { return fakeGroupFile{msg: &tg.Message{ID: id}} }

func ids(items []tfile.TGFileMessage) []int {
	out := make([]int, len(items))
	for i, item := range items {
		out[i] = messageID(item)
	}
	return out
}

func TestMergeGroupItemsKeepsSourceOrderWithoutDuplicates(t *testing.T) {
	got := ids(mergeGroupItems(
		[]tfile.TGFileMessage{groupFile(1059)},
		[]tfile.TGFileMessage{groupFile(1058), groupFile(1059), groupFile(1060)},
	))
	want := []int{1058, 1059, 1060}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestLateAlbumItemIsNotSavedAgainAfterDispatch(t *testing.T) {
	m := &MediaGroupHandler{dispatched: make(map[mediaGroupKey]dispatchedGroup)}
	key := mediaGroupKey{chatID: 1, userID: 1, groupID: 42}
	now := time.Now()
	m.markDispatched(key, []tfile.TGFileMessage{groupFile(1058), groupFile(1059)}, now)
	if !m.alreadyDispatched(key, 1059, now.Add(time.Second)) {
		t.Fatal("late update of an already saved album item must be skipped")
	}
	if m.alreadyDispatched(key, 1061, now.Add(time.Second)) {
		t.Fatal("an item that was not saved must not be skipped")
	}
	if m.alreadyDispatched(key, 1059, now.Add(dispatchedGroupTTL+time.Second)) {
		t.Fatal("dispatch records must expire")
	}
}

func TestMediaGroupTimeoutDefault(t *testing.T) {
	if got := mediaGroupTimeout(0); got != defaultMediaGroupTimeout {
		t.Fatalf("timeout 0 = %v, want %v", got, defaultMediaGroupTimeout)
	}
	if got := mediaGroupTimeout(5); got != 5*time.Second {
		t.Fatalf("timeout 5 = %v", got)
	}
}
