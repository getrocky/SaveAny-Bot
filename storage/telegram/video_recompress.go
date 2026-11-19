package telegram

import (
	"context"
	"errors"
	"fmt"
	"github.com/krau/SaveAny-Bot/common/cachegc"
	"io"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charmbracelet/log"
	"github.com/gabriel-vasile/mimetype"
	"github.com/krau/mygotg/ext"

	"github.com/krau/SaveAny-Bot/config"
	storconfig "github.com/krau/SaveAny-Bot/config/storage"
)

// Optional video recompression ("视频重压"). Large videos are re-encoded with
// libx264 at a visually lossless CRF before upload. When slicing is on as well,
// the segments are encoded in the same ffmpeg pass. The result is only used
// when it is clearly smaller than the source; otherwise the original (or plain
// stream-copy slices) is uploaded.

const (
	// recompressPreset keeps encoding tolerable on small 1-vCPU servers.
	recompressPreset = "veryfast"
	// recompressThreads bounds x264 memory use on low-RAM servers.
	recompressThreads = "2"
	// recompressMaxRatio: keep the output only if it is at most 90% of the source.
	recompressMaxRatio = 0.90
	// recompressDiskMargin is free space kept on top of the estimated output.
	recompressDiskMargin   = 200 << 20
	recompressAudioBitrate = "160k"
	recompressSegmentDelta = "0.300"
)

var (
	errRecompressNotSmaller = errors.New("recompressed video is not at least 10% smaller")
	errRecompressNoSpace    = errors.New("not enough free disk space to recompress")
)

// recompressOptions describes one recompression job; nil means disabled.
type recompressOptions struct {
	CRF int
}

// recompressOptionsFor returns options when recompression applies to a video of
// the given size, or nil.
func (t *Telegram) recompressOptionsFor(size int64) *recompressOptions {
	if t == nil || !t.config.RecompressVideo {
		return nil
	}
	if size <= t.config.EffectiveRecompressMinMB()*1024*1024 {
		return nil
	}
	return &recompressOptions{CRF: t.config.EffectiveRecompressCRF()}
}

// recompressExtension picks a container that can hold H.264: keep MKV, MP4,
// MOV and M4V, use MP4 for everything else (WebM, AVI, TS, ...).
func recompressExtension(filename string) string {
	switch ext := strings.ToLower(filepath.Ext(filepath.Base(filename))); ext {
	case ".mkv", ".mp4", ".mov", ".m4v":
		return ext
	default:
		return ".mp4"
	}
}

// withExtension replaces the extension of a file name or storage path.
func withExtension(name, extension string) string {
	if name == "" {
		return name
	}
	current := path.Ext(name)
	if strings.EqualFold(current, extension) {
		return name
	}
	return strings.TrimSuffix(name, current) + extension
}

// recompressTempBase returns the directory used for recompression output.
func recompressTempBase() (string, error) {
	tempBase := config.C().Temp.BasePath
	if tempBase == "" {
		tempBase = os.TempDir()
	}
	if err := os.MkdirAll(tempBase, 0o755); err != nil {
		return "", fmt.Errorf("failed to create recompress base directory: %w", err)
	}
	return tempBase, nil
}

// ensureRecompressSpace checks that dir has room for an output as large as the
// source plus a safety margin. Unknown free space (-1) is allowed.
func ensureRecompressSpace(dir string, sourceSize int64) error {
	free := diskFreeBytes(dir)
	if free < 0 {
		return nil
	}
	need := sourceSize + recompressDiskMargin
	if free < need {
		return fmt.Errorf("%w: %d MB free, need about %d MB", errRecompressNoSpace, free>>20, need>>20)
	}
	return nil
}

// recompressWorthIt reports whether the encoded size is small enough to use.
func recompressWorthIt(originalSize, encodedSize int64) bool {
	if originalSize <= 0 || encodedSize <= 0 {
		return false
	}
	return float64(encodedSize) <= float64(originalSize)*recompressMaxRatio
}

// recompressVideoArgs returns ffmpeg codec arguments. copyAudio selects audio
// stream copy, otherwise AAC.
func recompressVideoArgs(opts *recompressOptions, copyAudio bool) []string {
	args := []string{
		"-map", "0:v:0",
		"-map", "0:a?",
		"-sn", "-dn",
		"-map_metadata", "0",
		"-c:v", "libx264",
		"-preset", recompressPreset,
		"-crf", strconv.Itoa(opts.CRF),
		"-pix_fmt", "yuv420p",
		"-threads", recompressThreads,
	}
	if copyAudio {
		args = append(args, "-c:a", "copy")
	} else {
		args = append(args, "-c:a", "aac", "-b:a", recompressAudioBitrate)
	}
	return args
}

// runRecompressFFmpeg runs ffmpeg with lowered CPU priority so the bot stays
// responsive on a single core. It first tries audio stream copy and falls back
// to AAC when the container rejects the source audio codec.
func runRecompressFFmpeg(ctx context.Context, build func(copyAudio bool) []string, cleanup func()) error {
	err := runNiceMediaTool(ctx, build(true))
	if err == nil || ctx.Err() != nil {
		return err
	}
	log.FromContext(ctx).Infof("Recompress with audio copy failed, retrying with AAC: %v", err)
	cleanup()
	return runNiceMediaTool(ctx, build(false))
}

func runNiceMediaTool(ctx context.Context, args []string) error {
	if nicePath, err := exec.LookPath("nice"); err == nil {
		_, err := runMediaTool(ctx, nicePath, append([]string{"-n", "10", "ffmpeg"}, args...)...)
		return err
	}
	_, err := runMediaTool(ctx, "ffmpeg", args...)
	return err
}

// recompressWholeVideo encodes inputPath into a new file under the temp base.
// It returns the output path and size, or errRecompressNotSmaller /
// errRecompressNoSpace when the original should be uploaded instead.
func recompressWholeVideo(
	ctx context.Context,
	inputPath, filename string,
	sourceSize int64,
	opts *recompressOptions,
) (string, int64, func(), error) {
	noop := func() {}
	tempBase, err := recompressTempBase()
	if err != nil {
		return "", 0, noop, err
	}
	if err := ensureRecompressSpace(tempBase, sourceSize); err != nil {
		return "", 0, noop, err
	}
	sourceDuration, err := probeMediaDuration(ctx, inputPath)
	if err != nil {
		return "", 0, noop, fmt.Errorf("failed to probe source video duration: %w", err)
	}
	tempDir, err := os.MkdirTemp(tempBase, "telegram-video-recompress-*")
	if err != nil {
		return "", 0, noop, fmt.Errorf("failed to create recompress directory: %w", err)
	}
	releaseHold := cachegc.Hold(filepath.Base(tempDir))
	cleanup := func() {
		if err := os.RemoveAll(tempDir); err != nil {
			log.FromContext(ctx).Warnf("Failed to clean recompressed video: %s", err)
		}
		releaseHold()
	}
	extension := recompressExtension(filename)
	outputPath := filepath.Join(tempDir, "recompressed"+extension)
	// -fs stops ffmpeg once the output can no longer be worth it, so a
	// hopeless encode ends early instead of running to the end.
	limit := int64(math.Floor(float64(sourceSize) * recompressMaxRatio))
	build := func(copyAudio bool) []string {
		args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-i", inputPath}
		args = append(args, recompressVideoArgs(opts, copyAudio)...)
		args = append(args, "-fs", strconv.FormatInt(limit, 10))
		if extension != ".mkv" {
			args = append(args, "-movflags", "+faststart")
		}
		return append(args, outputPath)
	}
	if err := runRecompressFFmpeg(ctx, build, func() { _ = os.Remove(outputPath) }); err != nil {
		cleanup()
		return "", 0, noop, fmt.Errorf("ffmpeg recompress failed: %w", err)
	}
	info, err := os.Stat(outputPath)
	if err != nil {
		cleanup()
		return "", 0, noop, fmt.Errorf("failed to stat recompressed video: %w", err)
	}
	outDuration, err := probeMediaDuration(ctx, outputPath)
	if err != nil {
		cleanup()
		return "", 0, noop, fmt.Errorf("failed to validate recompressed video: %w", err)
	}
	// Hitting -fs truncates the output; a short duration means "not smaller".
	if outDuration < sourceDuration-1.5 || !recompressWorthIt(sourceSize, info.Size()) {
		cleanup()
		return "", 0, noop, errRecompressNotSmaller
	}
	return outputPath, info.Size(), cleanup, nil
}

// runFFmpegSegmentEncoded slices and re-encodes in one pass. Keyframes are
// forced at every segment boundary so each part starts cleanly.
func runFFmpegSegmentEncoded(
	ctx context.Context,
	inputPath, outputPattern, extension string,
	segmentDuration float64,
	opts *recompressOptions,
	cleanup func(),
) error {
	seg := strconv.FormatFloat(segmentDuration, 'f', 3, 64)
	build := func(copyAudio bool) []string {
		args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-i", inputPath}
		args = append(args, recompressVideoArgs(opts, copyAudio)...)
		args = append(args,
			"-force_key_frames", "expr:gte(t,n_forced*"+seg+")",
			"-f", "segment",
			"-segment_time", seg,
			// With B-frames the forced keyframe lands a frame or two after
			// the boundary in muxer time; without a delta the cut slips to
			// the next natural keyframe.
			"-segment_time_delta", recompressSegmentDelta,
			"-segment_start_number", "1",
			"-reset_timestamps", "1",
			"-avoid_negative_ts", "make_zero",
		)
		if extension != ".mkv" {
			args = append(args, "-segment_format_options", "movflags=+faststart")
		}
		return append(args, outputPattern)
	}
	return runRecompressFFmpeg(ctx, build, cleanup)
}

// splitVideoEncoded slices and recompresses inputPath in one ffmpeg pass. It
// returns errVideoShorterThanSegment / errVideoWithinSizeLimit exactly like the
// stream-copy split, and errRecompressNotSmaller or errRecompressNoSpace when
// the caller should fall back to stream-copy slicing.
func splitVideoEncoded(
	ctx context.Context,
	inputPath, outputDir, filename, mode string,
	segmentSec, maxPartBytes int64,
	opts *recompressOptions,
) ([]losslessVideoPart, error) {
	info, err := os.Stat(inputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to stat source video: %w", err)
	}
	sourceSize := info.Size()
	duration, err := probeMediaDuration(ctx, inputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to probe source video duration: %w", err)
	}
	var segmentDuration float64
	if mode == storconfig.SplitModeSize {
		if maxPartBytes <= 0 {
			return nil, fmt.Errorf("invalid video part size limit: %d", maxPartBytes)
		}
		if sourceSize <= maxPartBytes {
			return nil, errVideoWithinSizeLimit
		}
		segmentDuration = sizeSegmentDuration(duration, sourceSize, maxPartBytes)
	} else {
		segmentDuration = videoSplitDurationSec(segmentSec)
		if duration <= segmentDuration+0.05 {
			return nil, errVideoShorterThanSegment
		}
	}
	if segmentDuration < minSegmentDuration {
		return nil, fmt.Errorf("invalid video segment duration: %.3f", segmentDuration)
	}
	if err := ensureRecompressSpace(filepath.Dir(outputDir), sourceSize); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create recompress split directory: %w", err)
	}
	extension := recompressExtension(filename)
	pattern := filepath.Join(outputDir, "part-%03d"+extension)
	clear := func() { _ = clearVideoParts(outputDir) }
	if err := runFFmpegSegmentEncoded(ctx, inputPath, pattern, extension, segmentDuration, opts, clear); err != nil {
		return nil, fmt.Errorf("failed to recompress and split video: %w", err)
	}
	encodedName := withExtension(filepath.Base(filename), extension)
	parts, _, err := collectVideoParts(ctx, outputDir, encodedName)
	if err != nil {
		return nil, err
	}
	var total int64
	for _, part := range parts {
		total += part.Size
		if maxPartBytes > 0 && part.Size > maxPartBytes {
			return nil, fmt.Errorf("%w: part %s is %d bytes (limit %d)", errRecompressNotSmaller, part.Name, part.Size, maxPartBytes)
		}
	}
	if !recompressWorthIt(sourceSize, total) {
		return nil, fmt.Errorf("%w: %d -> %d bytes", errRecompressNotSmaller, sourceSize, total)
	}
	if len(parts) > maxLosslessVideoParts {
		return nil, fmt.Errorf("video split produced %d parts, exceeding the configured limit of %d", len(parts), maxLosslessVideoParts)
	}
	log.FromContext(ctx).Infof("Recompressed while slicing: %d -> %d bytes in %d parts", sourceSize, total, len(parts))
	return parts, nil
}

// recompressedVideo is a temporary recompressed copy ready for upload.
type recompressedVideo struct {
	file        *os.File
	size        int64
	storagePath string
	cleanup     func()
}

func (v *recompressedVideo) Close() {
	if v == nil {
		return
	}
	if v.file != nil {
		_ = v.file.Close()
	}
	if v.cleanup != nil {
		v.cleanup()
	}
}

// prepareRecompressedVideo recompresses a whole video when the option applies.
// It returns nil (and rewinds rs) when the original should be uploaded.
func (t *Telegram) prepareRecompressedVideo(
	ctx context.Context,
	tctx *ext.Context,
	rs io.ReadSeeker,
	storagePath string,
	size int64,
) (*recompressedVideo, error) {
	if size < 0 {
		if end, err := rs.Seek(0, io.SeekEnd); err == nil {
			size = end
		}
		if _, err := rs.Seek(0, io.SeekStart); err != nil {
			return nil, fmt.Errorf("failed to seek video source: %w", err)
		}
	}
	opts := t.recompressOptionsFor(size)
	if opts == nil {
		return nil, nil
	}
	filename, _ := t.target(tctx, path.Clean(storagePath))
	mimeType := ""
	if mtype, err := mimetype.DetectReader(rs); err == nil {
		mimeType = mtype.String()
	}
	if _, err := rs.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("failed to seek after mimetype detection: %w", err)
	}
	if !looksLikeVideo(mimeType, filename) && !looksLikeVideo(mimeType, storagePath) {
		return nil, nil
	}
	if filename == "" {
		filename = path.Base(storagePath)
	}
	inputPath, sourceCleanup, err := sourceFile(rs)
	if err != nil {
		return nil, fmt.Errorf("failed to prepare seekable video source: %w", err)
	}
	defer sourceCleanup()
	log.FromContext(ctx).Infof("Recompressing video %s (%d bytes, crf=%d, preset=%s)", filename, size, opts.CRF, recompressPreset)
	outputPath, outputSize, cleanup, err := recompressWholeVideo(ctx, inputPath, filename, size, opts)
	if _, seekErr := rs.Seek(0, io.SeekStart); seekErr != nil {
		cleanup()
		return nil, fmt.Errorf("failed to seek video after recompress: %w", seekErr)
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		log.FromContext(ctx).Infof("Recompress skipped, uploading original: %v", err)
		return nil, nil
	}
	file, err := os.Open(outputPath)
	if err != nil {
		cleanup()
		log.FromContext(ctx).Warnf("Recompress output unreadable, uploading original: %v", err)
		return nil, nil
	}
	log.FromContext(ctx).Infof("Recompressed video %s: %d -> %d bytes", filename, size, outputSize)
	return &recompressedVideo{
		file:        file,
		size:        outputSize,
		storagePath: withExtension(storagePath, recompressExtension(filename)),
		cleanup:     cleanup,
	}, nil
}
