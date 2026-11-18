package telegram

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/krau/SaveAny-Bot/config"
	storconfig "github.com/krau/SaveAny-Bot/config/storage"
)

func TestRecompressOptionsFor(t *testing.T) {
	tg := &Telegram{}
	if tg.recompressOptionsFor(1<<40) != nil {
		t.Fatal("recompression must be off by default")
	}
	tg.config.RecompressVideo = true
	if tg.recompressOptionsFor(200<<20) != nil {
		t.Fatal("200 MB is not above the default 200 MB threshold")
	}
	opts := tg.recompressOptionsFor(201 << 20)
	if opts == nil || opts.CRF != storconfig.DefaultRecompressCRF {
		t.Fatalf("expected default CRF options, got %+v", opts)
	}
	tg.config.RecompressMinMB = 10
	tg.config.RecompressCRF = 26
	if opts := tg.recompressOptionsFor(11 << 20); opts == nil || opts.CRF != 26 {
		t.Fatalf("expected configured options, got %+v", opts)
	}
}

func TestRecompressWorthIt(t *testing.T) {
	if !recompressWorthIt(1000, 900) {
		t.Fatal("exactly 10% smaller should be kept")
	}
	if recompressWorthIt(1000, 901) || recompressWorthIt(1000, 1200) || recompressWorthIt(0, 1) || recompressWorthIt(1000, 0) {
		t.Fatal("outputs that are not 10% smaller must be rejected")
	}
}

func TestRecompressExtension(t *testing.T) {
	cases := map[string]string{"a.MKV": ".mkv", "a.mp4": ".mp4", "a.mov": ".mov", "a.webm": ".mp4", "a.avi": ".mp4", "a": ".mp4"}
	for in, want := range cases {
		if got := recompressExtension(in); got != want {
			t.Fatalf("recompressExtension(%q) = %q, want %q", in, got, want)
		}
	}
	if got := withExtension("chat/dir/clip.webm", ".mp4"); got != "chat/dir/clip.mp4" {
		t.Fatalf("withExtension = %q", got)
	}
	if got := withExtension("clip.MP4", ".mp4"); got != "clip.MP4" {
		t.Fatalf("withExtension should keep same extension, got %q", got)
	}
}

func TestRecompressVideoArgs(t *testing.T) {
	args := strings.Join(recompressVideoArgs(&recompressOptions{CRF: 20}, true), " ")
	for _, want := range []string{"-c:v libx264", "-crf 20", "-preset veryfast", "-c:a copy", "-threads"} {
		if !strings.Contains(args, want) {
			t.Fatalf("args %q missing %q", args, want)
		}
	}
	if args := strings.Join(recompressVideoArgs(&recompressOptions{CRF: 20}, false), " "); !strings.Contains(args, "-c:a aac") {
		t.Fatalf("fallback args %q should use AAC", args)
	}
}

func TestEnsureRecompressSpace(t *testing.T) {
	dir := t.TempDir()
	if err := ensureRecompressSpace(dir, 1<<20); err != nil {
		t.Fatalf("small file should fit: %v", err)
	}
	if err := ensureRecompressSpace(dir, 1<<62); !errors.Is(err, errRecompressNoSpace) {
		t.Fatalf("huge file should report no space, got %v", err)
	}
}

// makeTestClip writes a short, high-bitrate clip so recompression shrinks it.
func makeTestClip(t *testing.T, seconds string, extraArgs ...string) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	cfg := config.Config{}
	cfg.Temp.BasePath = t.TempDir()
	config.Set(cfg)
	out := filepath.Join(t.TempDir(), "clip.mp4")
	args := []string{"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30:duration=" + seconds + ",noise=alls=8:allf=t",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=" + seconds,
		"-c:a", "aac", "-shortest"}
	if len(extraArgs) == 0 {
		extraArgs = []string{"-c:v", "libx264", "-preset", "ultrafast", "-qp", "0"}
	}
	args = append(args, extraArgs...)
	args = append(args, out)
	if output, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Skipf("cannot generate test clip: %v %s", err, output)
	}
	return out
}

func TestRecompressWholeVideo(t *testing.T) {
	clip := makeTestClip(t, "4")
	info, _ := os.Stat(clip)
	out, size, cleanup, err := recompressWholeVideo(context.Background(), clip, "clip.mp4", info.Size(), &recompressOptions{CRF: 20})
	if err != nil {
		t.Fatalf("recompress failed: %v", err)
	}
	defer cleanup()
	if size <= 0 || !recompressWorthIt(info.Size(), size) {
		t.Fatalf("expected a much smaller file: %d -> %d", info.Size(), size)
	}
	if _, err := probeMediaDuration(context.Background(), out); err != nil {
		t.Fatalf("output is not a valid video: %v", err)
	}
	cleanup()
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("cleanup should remove output, stat err=%v", err)
	}
}

func TestRecompressWholeVideoKeepsOriginalWhenNotSmaller(t *testing.T) {
	// Already tightly compressed: CRF 20 cannot make it 10% smaller.
	clip := makeTestClip(t, "3", "-c:v", "libx264", "-preset", "veryslow", "-crf", "40")
	info, _ := os.Stat(clip)
	_, _, _, err := recompressWholeVideo(context.Background(), clip, "clip.mp4", info.Size(), &recompressOptions{CRF: 20})
	if !errors.Is(err, errRecompressNotSmaller) {
		t.Fatalf("expected errRecompressNotSmaller, got %v", err)
	}
	entries, _ := os.ReadDir(config.C().Temp.BasePath)
	if len(entries) != 0 {
		t.Fatalf("temp files left behind: %d", len(entries))
	}
}

func TestSplitVideoEncodedInOnePass(t *testing.T) {
	clip := makeTestClip(t, "25")
	dir := filepath.Join(t.TempDir(), "enc")
	calls := 0
	original := runMediaTool
	t.Cleanup(func() { runMediaTool = original })
	runMediaTool = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "libx264") {
			calls++
		}
		return original(ctx, name, args...)
	}
	parts, err := splitVideoEncoded(context.Background(), clip, dir, "clip.mp4", storconfig.SplitModeDuration, 10, 0, &recompressOptions{CRF: 20})
	if err != nil {
		t.Fatalf("encoded split failed: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected one ffmpeg encode pass, got %d", calls)
	}
	if len(parts) != 3 {
		t.Fatalf("expected 3 parts for 25s / 10s, got %d", len(parts))
	}
	for i, part := range parts {
		want := "clip.part00" + string(rune('1'+i)) + ".mp4"
		if part.Name != want {
			t.Fatalf("part %d name = %q, want %q", i, part.Name, want)
		}
	}
	for _, part := range parts[:len(parts)-1] {
		d, err := probeMediaDuration(context.Background(), part.Path)
		if err != nil || d < 9.5 || d > 10.5 {
			t.Fatalf("part %s should be ~10s with forced keyframes, got %.2f (%v)", part.Name, d, err)
		}
	}
}

func TestSplitVideoEncodedShortVideo(t *testing.T) {
	clip := makeTestClip(t, "3")
	_, err := splitVideoEncoded(context.Background(), clip, filepath.Join(t.TempDir(), "enc"), "clip.mp4", storconfig.SplitModeDuration, 10, 0, &recompressOptions{CRF: 20})
	if !errors.Is(err, errVideoShorterThanSegment) {
		t.Fatalf("expected errVideoShorterThanSegment, got %v", err)
	}
}

func TestCreateVideoPartsFallsBackToCopyWhenNotSmaller(t *testing.T) {
	clip := makeTestClip(t, "25", "-c:v", "libx264", "-preset", "veryslow", "-crf", "40", "-g", "25")
	f, err := os.Open(clip)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	parts, cleanup, err := createVideoParts(context.Background(), f, "clip.mp4", storconfig.SplitModeDuration, 10, 0, &recompressOptions{CRF: 20})
	if err != nil {
		t.Fatalf("createVideoParts failed: %v", err)
	}
	defer cleanup()
	if len(parts) < 2 {
		t.Fatalf("expected stream-copy slices, got %d parts", len(parts))
	}
	for _, part := range parts {
		if strings.Contains(part.Path, string(filepath.Separator)+"recompress"+string(filepath.Separator)) {
			t.Fatalf("part %s should come from stream-copy slicing", part.Path)
		}
	}
}
