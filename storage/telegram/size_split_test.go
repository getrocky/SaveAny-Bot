package telegram

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	storconfig "github.com/krau/SaveAny-Bot/config/storage"
)

func TestSizeSegmentDuration(t *testing.T) {
	got := sizeSegmentDuration(600, 600<<20, 100<<20)
	want := 100 * sizeSplitSafety
	if got < want-0.001 || got > want+0.001 {
		t.Fatalf("sizeSegmentDuration = %f, want %f", got, want)
	}
	if sizeSegmentDuration(0, 1, 1) != 0 || sizeSegmentDuration(10, 0, 1) != 0 {
		t.Fatal("invalid inputs should yield 0")
	}
}

func TestEffectiveSplitMode(t *testing.T) {
	c := &storconfig.TelegramStorageConfig{}
	if c.EffectiveSplitMode() != storconfig.SplitModeDuration {
		t.Fatal("default mode should be duration")
	}
	c.SplitMode = storconfig.SplitModeSize
	if c.EffectiveSplitMode() != storconfig.SplitModeSize {
		t.Fatal("size mode not honored")
	}
	c.SplitMode = "zip"
	c.ChatID = 123456
	if err := c.Validate(); err == nil {
		t.Fatal("invalid split_mode should fail validation")
	}
}

func makeSizeTestVideo(t *testing.T, seconds string) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	out := filepath.Join(t.TempDir(), "src.mp4")
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=640x360:rate=25",
		"-f", "lavfi", "-i", "anoisesrc=d="+seconds,
		"-t", seconds, "-c:v", "libx264", "-preset", "ultrafast", "-g", "25", "-b:v", "2M",
		"-c:a", "aac", "-shortest", out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot generate test video: %v %s", err, b)
	}
	return out
}

func TestSplitVideoBySizeReal(t *testing.T) {
	src := makeSizeTestVideo(t, "20")
	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	limit := info.Size() / 4
	parts, err := splitVideoBySize(context.Background(), src, t.TempDir(), "clip.mp4", limit)
	if err != nil {
		t.Fatalf("splitVideoBySize: %v", err)
	}
	if len(parts) < 4 {
		t.Fatalf("expected >=4 parts, got %d", len(parts))
	}
	for i, p := range parts {
		if p.Size > limit {
			t.Fatalf("part %d is %d bytes > limit %d", i, p.Size, limit)
		}
		if filepath.Ext(p.Name) != ".mp4" {
			t.Fatalf("part %s is not a video file", p.Name)
		}
	}
	if _, err := splitVideoBySize(context.Background(), src, t.TempDir(), "clip.mp4", info.Size()+1); !errors.Is(err, errVideoWithinSizeLimit) {
		t.Fatalf("expected errVideoWithinSizeLimit, got %v", err)
	}
}

func TestDurationPartsResplitWhenTooLarge(t *testing.T) {
	src := makeSizeTestVideo(t, "12")
	info, _ := os.Stat(src)
	dir := t.TempDir()
	parts, err := splitVideoByDuration(context.Background(), src, dir, "clip.mp4", 6)
	if err != nil {
		t.Fatalf("splitVideoByDuration: %v", err)
	}
	limit := info.Size() * 2 / 5
	fitted, err := ensureVideoPartsUnder(context.Background(), parts, dir, "clip.mp4", limit)
	if err != nil {
		t.Fatalf("ensureVideoPartsUnder: %v", err)
	}
	if len(fitted) <= len(parts) {
		t.Fatalf("expected more parts after re-split: %d -> %d", len(parts), len(fitted))
	}
	for _, p := range fitted {
		if p.Size > limit {
			t.Fatalf("part %s too large: %d > %d", p.Name, p.Size, limit)
		}
	}
}

func TestLockTaskUploadsReentrant(t *testing.T) {
	tg := &Telegram{}
	tg.config.ChatID = -100987654321
	ctx, unlock := tg.LockTaskUploads(context.Background())
	done := make(chan struct{})
	go func() {
		_ = tg.withChatUploadLock(ctx, func(context.Context) error { return nil })
		close(done)
	}()
	<-done
	other := make(chan struct{})
	go func() {
		_, u := tg.LockTaskUploads(context.Background())
		u()
		close(other)
	}()
	time.Sleep(50 * time.Millisecond)
	select {
	case <-other:
		t.Fatal("another task acquired the chat lock while held")
	default:
	}
	unlock()
	<-other
}
