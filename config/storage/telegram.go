package storage

import (
	"fmt"

	"github.com/gotd/td/constant"
	storenum "github.com/krau/SaveAny-Bot/pkg/enums/storage"
)

const (
	SplitModeDuration = "duration"
	SplitModeSize     = "size"
)

type TelegramStorageConfig struct {
	BaseConfig
	ChatID    int64 `toml:"chat_id" mapstructure:"chat_id" json:"chat_id"`
	ForceFile bool  `toml:"force_file" mapstructure:"force_file" json:"force_file"`
	RateLimit int   `toml:"rate_limit" mapstructure:"rate_limit" json:"rate_limit"`
	RateBurst int   `toml:"rate_burst" mapstructure:"rate_burst" json:"rate_burst"`
	SkipLarge bool  `toml:"skip_large" mapstructure:"skip_large" json:"skip_large"` // skip files larger than Telegram limit(2GB)
	// SplitLargeVideo enables lossless video slicing via ffmpeg. Videos are
	// always cut into playable video parts, never ZIP volumes.
	SplitLargeVideo bool `toml:"split_large_video" mapstructure:"split_large_video" json:"split_large_video"`
	// SplitMode selects how videos are sliced: "duration" (default) or "size".
	SplitMode string `toml:"split_mode" mapstructure:"split_mode" json:"split_mode"`
	// SplitDurationSec is the target length of each video part in seconds.
	// 0 defaults to 120 (about 2 minutes). Only used when SplitLargeVideo is true.
	SplitDurationSec int64 `toml:"split_duration_sec" mapstructure:"split_duration_sec" json:"split_duration_sec"`
	// SplitSizeMB is the maximum size of each video part in MB when SplitMode
	// is "size". 0 uses the account upload limit.
	SplitSizeMB int64 `toml:"split_size_mb" mapstructure:"split_size_mb" json:"split_size_mb"`
	// Use metadata text as the caption when the live source caption is absent.
	UseMetadataCaption bool `toml:"use_metadata_caption" mapstructure:"use_metadata_caption" json:"use_metadata_caption"`
	// Use grouped_id from metadata to preserve source media groups when the live
	// source group key is absent.
	UseMetadataAlbum bool `toml:"use_metadata_album" mapstructure:"use_metadata_album" json:"use_metadata_album"`
}

func (m *TelegramStorageConfig) Validate() error {
	if m.ChatID == 0 {
		return fmt.Errorf("chat_id is required for telegram storage")
	}
	peerID := constant.TDLibPeerID(m.ChatID)
	if !peerID.IsUser() && !peerID.IsChat() && !peerID.IsChannel() {
		return fmt.Errorf("chat_id must be a Bot API style user or chat ID")
	}
	if m.RateLimit < 0 || m.RateBurst < 0 {
		return fmt.Errorf("rate_limit and rate_burst must be greater than 0 for telegram storage")
	}
	if m.SplitDurationSec < 0 {
		return fmt.Errorf("split_duration_sec must be >= 0 for telegram storage")
	}
	if m.SplitSizeMB < 0 {
		return fmt.Errorf("split_size_mb must be >= 0 for telegram storage")
	}
	switch m.SplitMode {
	case "", SplitModeDuration, SplitModeSize:
	default:
		return fmt.Errorf("split_mode must be %q or %q", SplitModeDuration, SplitModeSize)
	}
	return nil
}

func (m *TelegramStorageConfig) GetType() storenum.StorageType {
	return storenum.Telegram
}

func (m *TelegramStorageConfig) GetName() string {
	return m.Name
}

// EffectiveSplitMode returns the configured video split mode, defaulting to duration.
func (m *TelegramStorageConfig) EffectiveSplitMode() string {
	if m.SplitMode == SplitModeSize {
		return SplitModeSize
	}
	return SplitModeDuration
}
