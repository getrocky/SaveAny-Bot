package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/log"
	"github.com/gotd/td/telegram/message"
	"github.com/krau/mygotg/ext"
	"github.com/rs/xid"

	"github.com/krau/SaveAny-Bot/config"
	storconfig "github.com/krau/SaveAny-Bot/config/storage"
	"github.com/krau/SaveAny-Bot/pkg/consts/tglimit"
)

const (
	minSegmentDuration = 1.0
	// defaultVideoSplitDurationSec is used when split_duration_sec is 0.
	defaultVideoSplitDurationSec = 120
	// maxLosslessVideoParts caps runaway splits for tiny durations.
	// Parts are uploaded as sequential Telegram albums of MaxAlbumItems each.
	maxLosslessVideoParts = 500
)

type losslessVideoPart struct {
	Path string
	Name string
	Size int64
}

type mediaToolRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

var runMediaTool mediaToolRunner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s", err, message)
	}
	return output, nil
}

func videoSplitDurationSec(configured int64) float64 {
	if configured <= 0 {
		return float64(defaultVideoSplitDurationSec)
	}
	return float64(configured)
}

// createVideoParts slices a video into playable parts. mode is "duration" or
// "size". In duration mode, parts that still exceed maxPartBytes are re-split by
// size so every part can be uploaded. It never produces ZIP volumes.
func createVideoParts(
	ctx context.Context,
	r io.ReadSeeker,
	filename string,
	mode string,
	segmentSec int64,
	maxPartBytes int64,
) ([]losslessVideoPart, func(), error) {
	inputPath, sourceCleanup, err := sourceFile(r)
	if err != nil {
		return nil, func() {}, fmt.Errorf("failed to prepare seekable video source: %w", err)
	}

	tempBase := config.C().Temp.BasePath
	if tempBase == "" {
		tempBase = os.TempDir()
	}
	if err := os.MkdirAll(tempBase, 0o755); err != nil {
		sourceCleanup()
		return nil, func() {}, fmt.Errorf("failed to create video split base directory: %w", err)
	}
	tempDir, err := os.MkdirTemp(tempBase, "telegram-video-split-*")
	if err != nil {
		sourceCleanup()
		return nil, func() {}, fmt.Errorf("failed to create video split directory: %w", err)
	}
	cleanup := func() {
		if err := os.RemoveAll(tempDir); err != nil {
			log.FromContext(ctx).Warnf("Failed to clean video parts: %s", err)
		}
		sourceCleanup()
	}

	var parts []losslessVideoPart
	if mode == storconfig.SplitModeSize {
		parts, err = splitVideoBySize(ctx, inputPath, tempDir, filename, maxPartBytes)
	} else {
		parts, err = splitVideoByDuration(ctx, inputPath, tempDir, filename, videoSplitDurationSec(segmentSec))
		if errors.Is(err, errVideoShorterThanSegment) && maxPartBytes > 0 {
			if info, statErr := os.Stat(inputPath); statErr == nil && info.Size() > maxPartBytes {
				// Short but too large to upload as one file: fall back to size slicing.
				parts, err = splitVideoBySize(ctx, inputPath, tempDir, filename, maxPartBytes)
			}
		}
		if err == nil && maxPartBytes > 0 {
			parts, err = ensureVideoPartsUnder(ctx, parts, tempDir, filename, maxPartBytes)
		}
	}
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	return parts, cleanup, nil
}

// errVideoWithinSizeLimit means the source already fits in one part; caller
// should upload the original file.
var errVideoWithinSizeLimit = errors.New("video size does not exceed part size limit")

const (
	// sizeSplitSafety leaves headroom because stream-copy cuts happen on keyframes.
	sizeSplitSafety   = 0.85
	maxSizeSplitDepth = 3
)

// sizeSegmentDuration estimates the segment length that yields parts of at most
// maxPartBytes for a file of the given size and duration.
func sizeSegmentDuration(duration float64, size, maxPartBytes int64) float64 {
	if size <= 0 || maxPartBytes <= 0 || duration <= 0 {
		return 0
	}
	return duration * float64(maxPartBytes) / float64(size) * sizeSplitSafety
}

func splitVideoBySize(
	ctx context.Context,
	inputPath, outputDir, filename string,
	maxPartBytes int64,
) ([]losslessVideoPart, error) {
	if maxPartBytes <= 0 {
		return nil, fmt.Errorf("invalid video part size limit: %d", maxPartBytes)
	}
	info, err := os.Stat(inputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to stat source video: %w", err)
	}
	if info.Size() <= maxPartBytes {
		return nil, errVideoWithinSizeLimit
	}
	duration, err := probeMediaDuration(ctx, inputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to probe source video duration: %w", err)
	}
	paths, err := splitVideoToFit(ctx, inputPath, filepath.Join(outputDir, "size"), filename, duration, info.Size(), maxPartBytes, 0)
	if err != nil {
		return nil, err
	}
	parts, err := buildVideoParts(ctx, paths, filename)
	if err != nil {
		return nil, err
	}
	if len(parts) < 2 {
		return nil, fmt.Errorf("video size split produced %d part(s), expected at least 2", len(parts))
	}
	if len(parts) > maxLosslessVideoParts {
		return nil, fmt.Errorf("video split produced %d parts, exceeding the configured limit of %d", len(parts), maxLosslessVideoParts)
	}
	return parts, nil
}

// splitVideoToFit cuts inputPath into ordered parts no larger than maxPartBytes,
// re-splitting any oversized part (long keyframe intervals) a few times.
func splitVideoToFit(
	ctx context.Context,
	inputPath, outputDir, filename string,
	duration float64,
	size, maxPartBytes int64,
	depth int,
) ([]string, error) {
	segmentDuration := sizeSegmentDuration(duration, size, maxPartBytes)
	if segmentDuration < minSegmentDuration {
		return nil, fmt.Errorf("cannot slice video into parts of %d bytes: segment would be %.3fs", maxPartBytes, segmentDuration)
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create video split directory: %w", err)
	}
	extension := videoPartExtension(filename)
	var matches []string
	// Stream-copy cuts only land on keyframes; if a pass yields a single
	// oversized part, retry with shorter segments before giving up.
	for attempt := 0; ; attempt++ {
		if err := clearVideoParts(outputDir); err != nil {
			return nil, err
		}
		if err := runFFmpegSegment(ctx, inputPath, filepath.Join(outputDir, "part-%03d"+extension), extension, segmentDuration); err != nil {
			return nil, fmt.Errorf("failed to split video by size: %w", err)
		}
		found, err := filepath.Glob(filepath.Join(outputDir, "part-*"))
		if err != nil {
			return nil, fmt.Errorf("failed to list video parts: %w", err)
		}
		sort.Strings(found)
		if len(found) == 0 {
			return nil, fmt.Errorf("ffmpeg did not produce any video parts")
		}
		matches = found
		if len(found) > 1 || attempt >= 3 || segmentDuration/2 < minSegmentDuration {
			break
		}
		segmentDuration /= 2
	}
	out := make([]string, 0, len(matches))
	for index, match := range matches {
		info, err := os.Stat(match)
		if err != nil {
			return nil, fmt.Errorf("failed to stat video part %s: %w", match, err)
		}
		if info.Size() <= maxPartBytes {
			out = append(out, match)
			continue
		}
		if depth >= maxSizeSplitDepth || len(matches) == 1 {
			return nil, fmt.Errorf("video part %s is still %d bytes (limit %d) after re-splitting; keyframe interval too long", filepath.Base(match), info.Size(), maxPartBytes)
		}
		partDuration, err := probeMediaDuration(ctx, match)
		if err != nil {
			return nil, fmt.Errorf("failed to probe video part %s: %w", match, err)
		}
		children, err := splitVideoToFit(ctx, match, filepath.Join(outputDir, fmt.Sprintf("r%03d", index+1)), filename, partDuration, info.Size(), maxPartBytes, depth+1)
		if err != nil {
			return nil, err
		}
		out = append(out, children...)
	}
	return out, nil
}

// ensureVideoPartsUnder re-splits any duration part larger than maxPartBytes.
func ensureVideoPartsUnder(
	ctx context.Context,
	parts []losslessVideoPart,
	outputDir, filename string,
	maxPartBytes int64,
) ([]losslessVideoPart, error) {
	oversized := false
	for _, part := range parts {
		if part.Size > maxPartBytes {
			oversized = true
			break
		}
	}
	if !oversized {
		return parts, nil
	}
	paths := make([]string, 0, len(parts))
	for index, part := range parts {
		if part.Size <= maxPartBytes {
			paths = append(paths, part.Path)
			continue
		}
		duration, err := probeMediaDuration(ctx, part.Path)
		if err != nil {
			return nil, fmt.Errorf("failed to probe video part %s: %w", part.Name, err)
		}
		children, err := splitVideoToFit(ctx, part.Path, filepath.Join(outputDir, fmt.Sprintf("fit%03d", index+1)), filename, duration, part.Size, maxPartBytes, 1)
		if err != nil {
			return nil, err
		}
		paths = append(paths, children...)
	}
	result, err := buildVideoParts(ctx, paths, filename)
	if err != nil {
		return nil, err
	}
	if len(result) > maxLosslessVideoParts {
		return nil, fmt.Errorf("video split produced %d parts, exceeding the configured limit of %d", len(result), maxLosslessVideoParts)
	}
	return result, nil
}

// errVideoShorterThanSegment means the source fits in one segment; caller should
// upload the original file instead of parts.
var errVideoShorterThanSegment = errors.New("video duration does not exceed segment duration")

func splitVideoByDuration(
	ctx context.Context,
	inputPath, outputDir, filename string,
	segmentDuration float64,
) ([]losslessVideoPart, error) {
	if segmentDuration < minSegmentDuration {
		return nil, fmt.Errorf("invalid video segment duration: %.3f", segmentDuration)
	}

	duration, err := probeMediaDuration(ctx, inputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to probe source video duration: %w", err)
	}
	if duration < minSegmentDuration {
		return nil, fmt.Errorf("invalid source video duration: %.3f", duration)
	}
	// Allow a tiny probe epsilon so e.g. 120.01s still splits when segment is 120.
	if duration <= segmentDuration+0.05 {
		return nil, errVideoShorterThanSegment
	}

	extension := videoPartExtension(filename)
	outputPattern := filepath.Join(outputDir, "part-%03d"+extension)
	if err := clearVideoParts(outputDir); err != nil {
		return nil, err
	}
	if err := runFFmpegSegment(ctx, inputPath, outputPattern, extension, segmentDuration); err != nil {
		return nil, fmt.Errorf("failed to split video by duration: %w", err)
	}

	parts, _, err := collectVideoParts(ctx, outputDir, filename)
	if err != nil {
		return nil, err
	}
	if len(parts) < 2 {
		return nil, fmt.Errorf("video split produced %d part(s), expected at least 2", len(parts))
	}
	if len(parts) > maxLosslessVideoParts {
		return nil, fmt.Errorf(
			"video split produced %d parts, exceeding the configured limit of %d",
			len(parts),
			maxLosslessVideoParts,
		)
	}
	return parts, nil
}

func videoPartExtension(filename string) string {
	extension := strings.ToLower(filepath.Ext(filepath.Base(filename)))
	switch extension {
	case ".avi", ".m4v", ".mkv", ".mov", ".mp4", ".ts", ".webm":
		return extension
	default:
		return ".mp4"
	}
}

func runFFmpegSegment(
	ctx context.Context,
	inputPath, outputPattern, extension string,
	segmentDuration float64,
) error {
	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-nostdin",
		"-y",
		"-i", inputPath,
		"-map", "0",
		"-map_metadata", "0",
		"-c", "copy",
		"-f", "segment",
		"-segment_time", strconv.FormatFloat(segmentDuration, 'f', 3, 64),
		"-segment_start_number", "1",
		"-reset_timestamps", "1",
		"-avoid_negative_ts", "make_zero",
	}
	if extension == ".mp4" || extension == ".m4v" || extension == ".mov" {
		args = append(args, "-segment_format_options", "movflags=+faststart")
	}
	args = append(args, outputPattern)
	_, err := runMediaTool(ctx, "ffmpeg", args...)
	return err
}

func probeMediaDuration(ctx context.Context, filePath string) (float64, error) {
	output, err := runMediaTool(
		ctx,
		"ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		filePath,
	)
	if err != nil {
		return 0, err
	}
	duration, err := strconv.ParseFloat(strings.TrimSpace(string(output)), 64)
	if err != nil {
		return 0, fmt.Errorf("invalid ffprobe duration %q: %w", strings.TrimSpace(string(output)), err)
	}
	return duration, nil
}

func clearVideoParts(outputDir string) error {
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		return fmt.Errorf("failed to read video split directory: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(outputDir, entry.Name())); err != nil {
			return fmt.Errorf("failed to clear old video part %s: %w", entry.Name(), err)
		}
	}
	return nil
}

func collectVideoParts(
	ctx context.Context,
	outputDir, filename string,
) ([]losslessVideoPart, int64, error) {
	matches, err := filepath.Glob(filepath.Join(outputDir, "part-*"))
	if err != nil {
		return nil, 0, fmt.Errorf("failed to list video parts: %w", err)
	}
	sort.Strings(matches)
	if len(matches) == 0 {
		return nil, 0, fmt.Errorf("ffmpeg did not produce any video parts")
	}
	parts, err := buildVideoParts(ctx, matches, filename)
	if err != nil {
		return nil, 0, err
	}
	var largest int64
	for _, part := range parts {
		largest = max(largest, part.Size)
	}
	return parts, largest, nil
}

// buildVideoParts validates ordered part files and names them sequentially.
func buildVideoParts(ctx context.Context, paths []string, filename string) ([]losslessVideoPart, error) {
	extension := videoPartExtension(filename)
	base := strings.TrimSuffix(filepath.Base(filename), filepath.Ext(filepath.Base(filename)))
	if base == "" || base == "." {
		base = xid.New().String()
	}
	parts := make([]losslessVideoPart, 0, len(paths))
	for index, match := range paths {
		info, err := os.Stat(match)
		if err != nil {
			return nil, fmt.Errorf("failed to stat video part %s: %w", match, err)
		}
		if info.Size() <= 0 {
			return nil, fmt.Errorf("video part %s is empty", match)
		}
		if _, err := probeMediaDuration(ctx, match); err != nil {
			return nil, fmt.Errorf("failed to validate video part %s: %w", match, err)
		}
		parts = append(parts, losslessVideoPart{
			Path: match,
			Name: fmt.Sprintf("%s.part%03d%s", base, index+1, extension),
			Size: info.Size(),
		})
	}
	return parts, nil
}

func partStoragePath(storagePath, partName string) string {
	directory := path.Dir(path.Clean(storagePath))
	if directory == "." || directory == "/" {
		return partName
	}
	return path.Join(directory, partName)
}

func (t *Telegram) uploadLosslessVideoParts(
	ctx context.Context,
	tctx *ext.Context,
	storagePath string,
	parts []losslessVideoPart,
	sourceCaption *string,
	progress *uploadProgress,
) error {
	if len(parts) == 0 {
		return fmt.Errorf("no lossless video parts to upload")
	}
	if len(parts) > maxLosslessVideoParts {
		return fmt.Errorf(
			"refusing to upload %d lossless video parts; maximum is %d",
			len(parts),
			maxLosslessVideoParts,
		)
	}
	resetLosslessVideoUploadProgress(progress, parts)

	// Prepare and send one album (≤ MaxAlbumItems) at a time so part order stays
	// contiguous and we do not buffer every upload before the first send.
	for i := 0; i < len(parts); i += tglimit.MaxAlbumItems {
		end := min(i+tglimit.MaxAlbumItems, len(parts))
		batchParts := parts[i:end]
		prepared := make([]preparedMedia, 0, len(batchParts))
		for offset, part := range batchParts {
			index := i + offset
			partFile, err := os.Open(part.Path)
			if err != nil {
				return fmt.Errorf("failed to open video part %s: %w", part.Name, err)
			}
			media, prepareErr := t.prepareMedia(
				ctx,
				tctx,
				partFile,
				partStoragePath(storagePath, part.Name),
				part.Size,
				videoPartCaption(sourceCaption, index, len(parts)),
				progress,
			)
			closeErr := partFile.Close()
			if prepareErr != nil {
				return fmt.Errorf("failed to prepare video part %s: %w", part.Name, prepareErr)
			}
			if closeErr != nil {
				return fmt.Errorf("failed to close video part %s: %w", part.Name, closeErr)
			}
			prepared = append(prepared, *media)
		}

		builder := tctx.Sender.WithUploader(prepared[0].uploader).To(prepared[0].peer)
		if len(prepared) == 1 {
			if _, err := builder.Media(ctx, prepared[0].media); err != nil {
				return fmt.Errorf("failed to send video part %d: %w", i+1, err)
			}
			continue
		}
		media := make([]message.MultiMediaOption, len(prepared))
		for index := range prepared {
			media[index] = prepared[index].media
		}
		if _, err := builder.Album(ctx, media[0], media[1:]...); err != nil {
			return fmt.Errorf("failed to send video parts album starting at %d: %w", i+1, err)
		}
	}
	return nil
}

func resetLosslessVideoUploadProgress(progress *uploadProgress, parts []losslessVideoPart) {
	if progress == nil {
		return
	}
	var total int64
	for _, part := range parts {
		total += part.Size
	}
	progress.reset(total)
}

func videoPartCaption(sourceCaption *string, index, total int) *string {
	marker := fmt.Sprintf("(%d/%d)", index+1, total)
	if sourceCaption == nil || strings.TrimSpace(*sourceCaption) == "" {
		return &marker
	}
	if index == 0 {
		combined := strings.TrimSpace(*sourceCaption) + "\n" + marker
		return &combined
	}
	return &marker
}
