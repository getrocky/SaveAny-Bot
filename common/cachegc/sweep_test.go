package cachegc

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, dir, name string, size int, age time.Duration) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-age)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	return p
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestSweepRemovesOnlyIdleUnheld(t *testing.T) {
	dir := t.TempDir()
	idle := write(t, dir, "idleidleidle0001.mp4", 100, 8*time.Hour)
	fresh := write(t, dir, "freshfreshfresh01.mp4", 100, time.Minute)
	heldFile := write(t, dir, "heldheldheldheld1.mp4", 100, 8*time.Hour)
	queued := write(t, dir, "queuedqueued00001.mp4", 100, 8*time.Hour)

	release := Hold("heldheldheldheld1")
	defer release()
	res, err := Sweep(Options{
		Dir:      dir,
		IdleFor:  6 * time.Hour,
		IsActive: func(name string) bool { return name == "queuedqueued00001.mp4" },
	})
	if err != nil {
		t.Fatal(err)
	}
	if exists(idle) || !exists(fresh) || !exists(heldFile) || !exists(queued) {
		t.Fatalf("unexpected state idle=%v fresh=%v held=%v queued=%v", exists(idle), exists(fresh), exists(heldFile), exists(queued))
	}
	if res.Removed != 1 || res.Bytes != 100 || res.Kept != 3 {
		t.Fatalf("result = %+v", res)
	}
}

func TestSweepKeepsResumeGroupTogether(t *testing.T) {
	dir := t.TempDir()
	// Data is old, but the bitmap was touched recently: the whole group stays.
	part := write(t, dir, "taskidtaskid0002.mp4.part", 50, 9*time.Hour)
	bitmap := write(t, dir, "taskidtaskid0002.mp4.part.bitmap", 5, time.Minute)
	if _, err := Sweep(Options{Dir: dir, IdleFor: 6 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	if !exists(part) || !exists(bitmap) {
		t.Fatal("resume group must be kept while any member is recent")
	}
	// Once all of it is idle, it goes together.
	old := time.Now().Add(-8 * time.Hour)
	_ = os.Chtimes(bitmap, old, old)
	res, _ := Sweep(Options{Dir: dir, IdleFor: 6 * time.Hour})
	if exists(part) || exists(bitmap) || res.Removed != 1 || res.Bytes != 55 {
		t.Fatalf("group not removed together: %+v", res)
	}
}

func TestSweepHeldGroupKeepsSidecars(t *testing.T) {
	dir := t.TempDir()
	part := write(t, dir, "elementidelement1.mp4.part", 10, 9*time.Hour)
	bitmap := write(t, dir, "elementidelement1.mp4.part.bitmap", 1, 9*time.Hour)
	defer Hold("elementidelement1")()
	if _, err := Sweep(Options{Dir: dir, IdleFor: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if !exists(part) || !exists(bitmap) {
		t.Fatal("held element files were removed")
	}
}

func TestSweepDirectoriesUseNewestChild(t *testing.T) {
	dir := t.TempDir()
	busy := filepath.Join(dir, "telegram-video-split-111")
	idle := filepath.Join(dir, "telegram-video-split-222")
	for _, d := range []string{busy, idle} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-9 * time.Hour)
	write(t, busy, "part1.mp4", 10, time.Minute) // recent child keeps the dir
	write(t, idle, "part1.mp4", 10, 9*time.Hour)
	for _, d := range []string{busy, idle} {
		_ = os.Chtimes(d, old, old)
	}
	res, err := Sweep(Options{Dir: dir, IdleFor: 6 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if !exists(busy) || exists(idle) || res.Removed != 1 || res.Bytes != 10 {
		t.Fatalf("busy=%v idle=%v res=%+v", exists(busy), exists(idle), res)
	}
}

func TestSweepSkipsSymlinksAndRequiresAbs(t *testing.T) {
	dir := t.TempDir()
	outside := write(t, t.TempDir(), "secretsecretsecret", 10, 9*time.Hour)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("symlinks unsupported")
	}
	if _, err := Sweep(Options{Dir: dir, IdleFor: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if !exists(outside) {
		t.Fatal("symlink target removed")
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatal("symlink itself should be left alone")
	}
	if _, err := Sweep(Options{Dir: "relative", IdleFor: time.Hour}); err == nil {
		t.Fatal("relative dir must be rejected")
	}
}

func TestHoldRefCountAndMinLength(t *testing.T) {
	r1 := Hold("abcdefghijkl")
	r2 := Hold("abcdefghijkl")
	r1()
	r1() // idempotent
	if !IsHeld("x-abcdefghijkl.mp4") {
		t.Fatal("second hold lost")
	}
	r2()
	if IsHeld("x-abcdefghijkl.mp4") {
		t.Fatal("hold not released")
	}
	defer Hold("short")()
	if IsHeld("short") {
		t.Fatal("short keys must be ignored")
	}
}

func TestResolveDirRefusesWorkdirAndAncestors(t *testing.T) {
	wd, _ := os.Getwd()
	for _, p := range []string{wd, filepath.Dir(wd), "/", "", "."} {
		if _, err := ResolveDir(p); err == nil {
			t.Fatalf("ResolveDir(%q) should fail", p)
		}
	}
	got, err := ResolveDir("cache/")
	if err != nil || got != filepath.Join(wd, "cache") {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestGroupKey(t *testing.T) {
	cases := map[string]string{
		"a.mp4":             "a.mp4",
		"a.mp4.part":        "a.mp4",
		"a.mp4.part.bitmap": "a.mp4",
		"a.mp4.relay":       "a.mp4",
		".part":             ".part",
	}
	for in, want := range cases {
		if got := groupKey(in); got != want {
			t.Errorf("groupKey(%q)=%q want %q", in, got, want)
		}
	}
}
