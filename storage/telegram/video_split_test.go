package telegram

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gotd/td/telegram/uploader"

	"github.com/krau/SaveAny-Bot/pkg/consts/tglimit"
)

func TestVideoPartExtension(t *testing.T) {
	cases := map[string]string{
		"clip.MP4":  ".mp4",
		"clip.mov":  ".mov",
		"clip.mkv":  ".mkv",
		"clip.webm": ".webm",
		"clip.bin":  ".mp4",
		"clip":      ".mp4",
	}
	for input, want := range cases {
		if got := videoPartExtension(input); got != want {
			t.Fatalf("videoPartExtension(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestPartStoragePath(t *testing.T) {
	cases := map[string]string{
		"video.mp4":          "part.mp4",
		"/chat/video.mp4":    "/chat/part.mp4",
		"chat/dir/video.mp4": "chat/dir/part.mp4",
	}
	for input, want := range cases {
		got := partStoragePath(input, "part.mp4")
		if got != want {
			t.Fatalf("partStoragePath(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestVideoPartCaption(t *testing.T) {
	if got := videoPartCaption(nil, 0, 3); got == nil || *got != "(1/3)" {
		t.Fatalf("caption without source = %v, want (1/3)", got)
	}

	source := "original caption"
	first := videoPartCaption(&source, 0, 3)
	wantFirst := "original caption\n(1/3)"
	if first == nil || *first != wantFirst {
		t.Fatalf("first part caption = %q, want %q", deref(first), wantFirst)
	}
	second := videoPartCaption(&source, 1, 3)
	if second == nil || *second != "(2/3)" {
		t.Fatalf("second part caption = %v, want (2/3)", second)
	}
}

func deref(v *string) string {
	if v == nil {
		return "<nil>"
	}
	return *v
}

func TestResetLosslessVideoUploadProgressUsesCombinedPartSize(t *testing.T) {
	var uploaded, total int64
	progress := newUploadProgress(999, func(current, size int64) {
		uploaded = current
		total = size
	})
	resetLosslessVideoUploadProgress(progress, []losslessVideoPart{
		{Size: 100},
		{Size: 250},
	})

	if err := progress.Chunk(t.Context(), uploader.ProgressState{ID: 1, Uploaded: 50, Total: 100}); err != nil {
		t.Fatalf("Chunk() failed: %v", err)
	}
	if uploaded != 50 || total != 350 {
		t.Fatalf("lossless video progress = %d/%d, want 50/350", uploaded, total)
	}
}

func TestVideoSplitDurationSecDefault(t *testing.T) {
	if got := videoSplitDurationSec(0); got != 120 {
		t.Fatalf("default = %v, want 120", got)
	}
	if got := videoSplitDurationSec(90); got != 90 {
		t.Fatalf("custom = %v, want 90", got)
	}
}

func TestSplitVideoByDuration(t *testing.T) {
	tempDir := t.TempDir()
	inputPath := filepath.Join(tempDir, "source.mov")
	if err := os.WriteFile(inputPath, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	outputDir := filepath.Join(tempDir, "parts")
	if err := os.Mkdir(outputDir, 0o755); err != nil {
		t.Fatal(err)
	}

	originalRunner := runMediaTool
	t.Cleanup(func() { runMediaTool = originalRunner })
	runMediaTool = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "ffprobe":
			return []byte("300.0\n"), nil
		case "ffmpeg":
			// ensure segment_time is the configured duration
			joined := strings.Join(args, " ")
			if !strings.Contains(joined, "-segment_time 120.000") {
				return nil, fmt.Errorf("missing segment_time 120 in %v", args)
			}
			pattern := args[len(args)-1]
			for index := range 3 {
				partPath := strings.Replace(pattern, "%03d", fmt.Sprintf("%03d", index+1), 1)
				if err := os.WriteFile(partPath, make([]byte, 100), 0o600); err != nil {
					return nil, err
				}
			}
			return nil, nil
		default:
			return nil, fmt.Errorf("unexpected media tool %q", name)
		}
	}

	parts, err := splitVideoByDuration(t.Context(), inputPath, outputDir, "example.mov", 120)
	if err != nil {
		t.Fatalf("splitVideoByDuration() failed: %v", err)
	}
	if len(parts) != 3 {
		t.Fatalf("parts = %d, want 3", len(parts))
	}
	if parts[0].Name != "example.part001.mov" {
		t.Fatalf("unexpected first part name: %s", parts[0].Name)
	}
}

func TestSplitVideoByDurationSkipsShortVideo(t *testing.T) {
	tempDir := t.TempDir()
	inputPath := filepath.Join(tempDir, "source.mov")
	if err := os.WriteFile(inputPath, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	outputDir := filepath.Join(tempDir, "parts")
	if err := os.Mkdir(outputDir, 0o755); err != nil {
		t.Fatal(err)
	}

	originalRunner := runMediaTool
	t.Cleanup(func() { runMediaTool = originalRunner })
	runMediaTool = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "ffprobe" {
			return []byte("90.0\n"), nil
		}
		return nil, fmt.Errorf("unexpected media tool %q", name)
	}

	_, err := splitVideoByDuration(t.Context(), inputPath, outputDir, "example.mov", 120)
	if !errors.Is(err, errVideoShorterThanSegment) {
		t.Fatalf("error = %v, want errVideoShorterThanSegment", err)
	}
}

func TestSplitVideoByDurationAllowsMultipleAlbums(t *testing.T) {
	tempDir := t.TempDir()
	inputPath := filepath.Join(tempDir, "source.mov")
	if err := os.WriteFile(inputPath, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	outputDir := filepath.Join(tempDir, "parts")
	if err := os.Mkdir(outputDir, 0o755); err != nil {
		t.Fatal(err)
	}

	originalRunner := runMediaTool
	t.Cleanup(func() { runMediaTool = originalRunner })
	partCount := tglimit.MaxAlbumItems + 3
	runMediaTool = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "ffprobe":
			return []byte("3600.0\n"), nil
		case "ffmpeg":
			pattern := args[len(args)-1]
			for index := range partCount {
				partPath := strings.Replace(pattern, "%03d", fmt.Sprintf("%03d", index+1), 1)
				if err := os.WriteFile(partPath, make([]byte, 100), 0o600); err != nil {
					return nil, err
				}
			}
			return nil, nil
		default:
			return nil, fmt.Errorf("unexpected media tool %q", name)
		}
	}

	parts, err := splitVideoByDuration(t.Context(), inputPath, outputDir, "example.mov", 120)
	if err != nil {
		t.Fatalf("splitVideoByDuration() failed for multi-album parts: %v", err)
	}
	if len(parts) != partCount {
		t.Fatalf("parts = %d, want %d", len(parts), partCount)
	}
}

func TestSplitVideoByDurationRejectsExcessiveParts(t *testing.T) {
	tempDir := t.TempDir()
	inputPath := filepath.Join(tempDir, "source.mov")
	if err := os.WriteFile(inputPath, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	outputDir := filepath.Join(tempDir, "parts")
	if err := os.Mkdir(outputDir, 0o755); err != nil {
		t.Fatal(err)
	}

	originalRunner := runMediaTool
	t.Cleanup(func() { runMediaTool = originalRunner })
	runMediaTool = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "ffprobe":
			return []byte("99999.0\n"), nil
		case "ffmpeg":
			pattern := args[len(args)-1]
			for index := range maxLosslessVideoParts + 1 {
				partPath := strings.Replace(pattern, "%03d", fmt.Sprintf("%03d", index+1), 1)
				if err := os.WriteFile(partPath, make([]byte, 100), 0o600); err != nil {
					return nil, err
				}
			}
			return nil, nil
		default:
			return nil, fmt.Errorf("unexpected media tool %q", name)
		}
	}

	_, err := splitVideoByDuration(t.Context(), inputPath, outputDir, "example.mov", 120)
	if err == nil {
		t.Fatal("splitVideoByDuration() succeeded beyond maxLosslessVideoParts")
	}
	if !strings.Contains(err.Error(), "configured limit") {
		t.Fatalf("splitVideoByDuration() error = %q, want configured limit", err)
	}
}

func TestLooksLikeVideo(t *testing.T) {
	cases := []struct {
		mime, name string
		want       bool
	}{
		{"video/mp4", "x.bin", true},
		{"application/octet-stream", "clip.mp4", true},
		{"application/octet-stream", "CLIP.MOV", true},
		{"image/jpeg", "a.jpg", false},
		{"application/pdf", "a.pdf", false},
		{"", "movie.mkv", true},
		{"", "noext", false},
	}
	for _, tc := range cases {
		if got := looksLikeVideo(tc.mime, tc.name); got != tc.want {
			t.Fatalf("looksLikeVideo(%q,%q)=%v want %v", tc.mime, tc.name, got, tc.want)
		}
	}
}
