package telegram

import (
	"bytes"
	"testing"

	storconfig "github.com/krau/SaveAny-Bot/config/storage"
	"github.com/krau/SaveAny-Bot/pkg/storagetypes"
)

func TestInspectBatchItemRoutesVideosToSingleSaveWhenDurationSplitEnabled(t *testing.T) {
	tg := &Telegram{config: storconfig.TelegramStorageConfig{
		ChatID:           8403929176,
		SplitLargeVideo:  true,
		SplitDurationSec: 119,
	}}

	// Minimal MP4-like ftyp box so mimetype detects video/mp4
	payload := append([]byte{
		0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p',
		'i', 's', 'o', 'm', 0x00, 0x00, 0x02, 0x00,
		'i', 's', 'o', 'm', 'i', 's', 'o', '2',
	}, bytes.Repeat([]byte{0x00}, 64)...)

	item := storagetypes.BatchItem{
		Reader:      bytes.NewReader(payload),
		Size:        int64(len(payload)),
		StoragePath: "sample.mp4",
	}
	got, err := tg.inspectBatchItem(nil, item)
	if err != nil {
		t.Fatalf("inspectBatchItem: %v", err)
	}
	if !got.useSingleSave {
		t.Fatalf("useSingleSave=false, want true so SaveBatch routes through duration split")
	}
}

func TestInspectBatchItemKeepsAlbumForImagesWhenDurationSplitEnabled(t *testing.T) {
	tg := &Telegram{config: storconfig.TelegramStorageConfig{
		ChatID:           8403929176,
		SplitLargeVideo:  true,
		SplitDurationSec: 119,
	}}
	jpeg := []byte{
		0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01, 0x01, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00,
		0xff, 0xd9,
	}
	item := storagetypes.BatchItem{
		Reader:      bytes.NewReader(jpeg),
		Size:        int64(len(jpeg)),
		StoragePath: "pic.jpg",
	}
	got, err := tg.inspectBatchItem(nil, item)
	if err != nil {
		t.Fatalf("inspectBatchItem: %v", err)
	}
	if got.useSingleSave {
		t.Fatalf("image should not force single-save for duration split")
	}
}
