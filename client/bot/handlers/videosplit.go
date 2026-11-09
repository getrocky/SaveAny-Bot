package handlers

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/telegram/message/markup"
	"github.com/gotd/td/tg"
	"github.com/krau/SaveAny-Bot/client/bot/handlers/utils/msgelem"
	"github.com/krau/SaveAny-Bot/config"
	storconfig "github.com/krau/SaveAny-Bot/config/storage"
	"github.com/krau/SaveAny-Bot/pkg/tcbdata"
	"github.com/krau/mygotg/dispatcher"
	"github.com/krau/mygotg/ext"
)

// Telegram「视频切片」menu: per Telegram storage, toggle video slicing, pick
// slicing by seconds or by size (MB), and enter the value. Changes are saved
// through the same storage settings the web admin uses, then hot-reloaded.

const (
	videoSplitInputTTL = 5 * time.Minute

	videoSplitMinSec = 10
	videoSplitMaxSec = 3600
	videoSplitMinMB  = 1
	videoSplitMaxMB  = 2000
)

// StorageConfigUpdater merges patch into the storage config with the given ID,
// persists it, and reloads storages.
type StorageConfigUpdater func(ctx context.Context, storageID uint, patch map[string]any) error

var (
	storageUpdaterMu     sync.RWMutex
	storageConfigUpdater StorageConfigUpdater
)

// SetStorageConfigUpdater wires the settings service into the bot menu.
func SetStorageConfigUpdater(fn StorageConfigUpdater) {
	storageUpdaterMu.Lock()
	storageConfigUpdater = fn
	storageUpdaterMu.Unlock()
}

func updateStorageConfig(ctx context.Context, storageID uint, patch map[string]any) error {
	storageUpdaterMu.RLock()
	fn := storageConfigUpdater
	storageUpdaterMu.RUnlock()
	if fn == nil {
		return errors.New("storage settings service is not available")
	}
	return fn(ctx, storageID, patch)
}

type videoSplitPending struct {
	storageID uint
	mode      string
	msgID     int
	expires   time.Time
}

type videoSplitInputStore struct {
	mu      sync.Mutex
	pending map[string]videoSplitPending
	now     func() time.Time
}

var videoSplitInputs = &videoSplitInputStore{pending: map[string]videoSplitPending{}, now: time.Now}

func videoSplitKey(userID, chat int64) string { return fmt.Sprintf("%d:%d", userID, chat) }

func (s *videoSplitInputStore) set(userID, chat int64, p videoSplitPending) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p.expires = s.now().Add(videoSplitInputTTL)
	s.pending[videoSplitKey(userID, chat)] = p
}

func (s *videoSplitInputStore) clear(userID, chat int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, videoSplitKey(userID, chat))
}

// take returns and removes a live pending input; expired entries are dropped.
func (s *videoSplitInputStore) take(userID, chat int64) (videoSplitPending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := videoSplitKey(userID, chat)
	p, ok := s.pending[key]
	if !ok {
		return p, false
	}
	delete(s.pending, key)
	if s.now().After(p.expires) {
		return p, false
	}
	return p, true
}

// parseVideoSplitValue validates a seconds or MB value typed by the user.
func parseVideoSplitValue(mode, text string) (int64, error) {
	text = strings.TrimSpace(text)
	text = strings.TrimSuffix(strings.TrimSuffix(strings.ToLower(text), "mb"), "s")
	text = strings.TrimSuffix(strings.TrimSuffix(text, "秒"), "m")
	value, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	if err != nil {
		return 0, errors.New("请输入整数")
	}
	if mode == storconfig.SplitModeSize {
		if value < videoSplitMinMB || value > videoSplitMaxMB {
			return 0, fmt.Errorf("大小需在 %d–%d MB 之间", videoSplitMinMB, videoSplitMaxMB)
		}
		return value, nil
	}
	if value < videoSplitMinSec || value > videoSplitMaxSec {
		return 0, fmt.Errorf("秒数需在 %d–%d 之间", videoSplitMinSec, videoSplitMaxSec)
	}
	return value, nil
}

// userTelegramStorages returns Telegram storage configs the user may use.
func userTelegramStorages(userID int64) []*storconfig.TelegramStorageConfig {
	cfg := config.C()
	allowed := map[uint]struct{}{}
	for _, id := range cfg.GetStorageIDsByUserID(userID) {
		allowed[id] = struct{}{}
	}
	var out []*storconfig.TelegramStorageConfig
	for _, st := range cfg.Storages {
		tgCfg, ok := st.(*storconfig.TelegramStorageConfig)
		if !ok {
			continue
		}
		if _, ok := allowed[tgCfg.GetID()]; ok {
			out = append(out, tgCfg)
		}
	}
	return out
}

func userTelegramStorage(userID int64, id uint) *storconfig.TelegramStorageConfig {
	for _, st := range userTelegramStorages(userID) {
		if st.GetID() == id {
			return st
		}
	}
	return nil
}

func videoSplitModeLabel(mode string) string {
	if mode == storconfig.SplitModeSize {
		return "按大小"
	}
	return "按秒数"
}

func videoSplitValueLabel(st *storconfig.TelegramStorageConfig) string {
	if st.EffectiveSplitMode() == storconfig.SplitModeSize {
		if st.SplitSizeMB <= 0 {
			return "每段不超过上传上限"
		}
		return fmt.Sprintf("每段不超过 %d MB", st.SplitSizeMB)
	}
	sec := st.SplitDurationSec
	if sec <= 0 {
		sec = 120
	}
	return fmt.Sprintf("每段 %d 秒", sec)
}

func videoSplitDetailText(st *storconfig.TelegramStorageConfig, extra string) string {
	state := "已关闭"
	if st.SplitLargeVideo {
		state = "已开启"
	}
	text := fmt.Sprintf("🎬 视频切片 · %s\n\n状态：%s\n方式：%s\n设置：%s\n\n切出来的都是可直接播放的视频，不会打成 ZIP 分卷。",
		st.GetName(), state, videoSplitModeLabel(st.EffectiveSplitMode()), videoSplitValueLabel(st))
	if extra != "" {
		text += "\n\n" + extra
	}
	return text
}

func vsplitData(parts ...any) []byte {
	args := []string{tcbdata.TypeConfig, "vsplit"}
	for _, p := range parts {
		args = append(args, fmt.Sprint(p))
	}
	return []byte(strings.Join(args, " "))
}

func videoSplitDetailMarkup(st *storconfig.TelegramStorageConfig) *tg.ReplyInlineMarkup {
	id := st.GetID()
	toggle := "✅ 开启切片"
	if st.SplitLargeVideo {
		toggle = "⏸ 关闭切片"
	}
	durLabel, sizeLabel := "按秒数", "按大小"
	if st.EffectiveSplitMode() == storconfig.SplitModeSize {
		sizeLabel = "• 按大小"
	} else {
		durLabel = "• 按秒数"
	}
	inputLabel := "✏️ 设置秒数"
	if st.EffectiveSplitMode() == storconfig.SplitModeSize {
		inputLabel = "✏️ 设置大小(MB)"
	}
	return &tg.ReplyInlineMarkup{Rows: []tg.KeyboardInlineButtonRow{
		{Buttons: []tg.KeyboardInlineButton{markup.Callback(toggle, vsplitData("t", id))}},
		{Buttons: []tg.KeyboardInlineButton{
			markup.Callback(durLabel, vsplitData("m", id, storconfig.SplitModeDuration)),
			markup.Callback(sizeLabel, vsplitData("m", id, storconfig.SplitModeSize)),
		}},
		{Buttons: []tg.KeyboardInlineButton{markup.Callback(inputLabel, vsplitData("i", id))}},
		{Buttons: []tg.KeyboardInlineButton{
			markup.Callback("⬅️ 返回存储列表", vsplitData()),
			markup.Callback("🏠 设置主菜单", []byte(tcbdata.TypeConfig+" menu")),
		}},
	}}
}

func videoSplitListMarkup(storages []*storconfig.TelegramStorageConfig) *tg.ReplyInlineMarkup {
	rows := make([]tg.KeyboardInlineButtonRow, 0, len(storages)+1)
	for _, st := range storages {
		state := "关"
		if st.SplitLargeVideo {
			state = "开 · " + videoSplitModeLabel(st.EffectiveSplitMode())
		}
		rows = append(rows, tg.KeyboardInlineButtonRow{Buttons: []tg.KeyboardInlineButton{
			markup.Callback(fmt.Sprintf("%s（%s）", st.GetName(), state), vsplitData("s", st.GetID())),
		}})
	}
	rows = append(rows, tg.KeyboardInlineButtonRow{Buttons: []tg.KeyboardInlineButton{
		markup.Callback("⬅️ 返回", []byte(tcbdata.TypeConfig+" menu")),
	}})
	return &tg.ReplyInlineMarkup{Rows: rows}
}

func editVideoSplit(ctx *ext.Context, update *ext.Update, text string, mk *tg.ReplyInlineMarkup) {
	ctx.EditMessage(chatID(update), &tg.MessagesEditMessageRequest{
		ID:          update.CallbackQuery.GetMsgID(),
		Message:     text,
		ReplyMarkup: mk,
	})
}

func handleConfigVideoSplitCallback(ctx *ext.Context, update *ext.Update) error {
	userID := update.CallbackQuery.GetUserID()
	queryID := update.CallbackQuery.GetQueryID()
	args := strings.Fields(string(update.CallbackQuery.Data))
	alert := func(msg string) error {
		ctx.AnswerCallback(msgelem.AlertCallbackAnswer(queryID, msg))
		return dispatcher.EndGroups
	}

	if len(args) <= 2 {
		videoSplitInputs.clear(userID, chatID(update))
		storages := userTelegramStorages(userID)
		if len(storages) == 0 {
			return alert("你没有可用的 Telegram 存储")
		}
		editVideoSplit(ctx, update, "🎬 视频切片\n\n选择要设置的 Telegram 存储：", videoSplitListMarkup(storages))
		return dispatcher.EndGroups
	}
	if len(args) < 4 {
		return alert("无效的按钮数据")
	}
	id64, err := strconv.ParseUint(args[3], 10, 64)
	if err != nil {
		return alert("无效的存储")
	}
	st := userTelegramStorage(userID, uint(id64))
	if st == nil {
		return alert("你没有这个存储的权限，或它不是 Telegram 存储")
	}
	id := st.GetID()

	save := func(patch map[string]any, note string) error {
		if err := updateStorageConfig(ctx, id, patch); err != nil {
			return alert("保存失败：" + err.Error())
		}
		if fresh := userTelegramStorage(userID, id); fresh != nil {
			st = fresh
		}
		editVideoSplit(ctx, update, videoSplitDetailText(st, note), videoSplitDetailMarkup(st))
		return dispatcher.EndGroups
	}

	switch args[2] {
	case "s":
		videoSplitInputs.clear(userID, chatID(update))
		editVideoSplit(ctx, update, videoSplitDetailText(st, ""), videoSplitDetailMarkup(st))
		return dispatcher.EndGroups
	case "t":
		next := !st.SplitLargeVideo
		note := "已关闭视频切片"
		if next {
			note = "已开启视频切片"
		}
		return save(map[string]any{"split_large_video": next}, note)
	case "m":
		if len(args) < 5 || (args[4] != storconfig.SplitModeDuration && args[4] != storconfig.SplitModeSize) {
			return alert("无效的切片方式")
		}
		return save(map[string]any{"split_mode": args[4]}, "切片方式已改为"+videoSplitModeLabel(args[4]))
	case "i":
		mode := st.EffectiveSplitMode()
		videoSplitInputs.set(userID, chatID(update), videoSplitPending{storageID: id, mode: mode, msgID: update.CallbackQuery.GetMsgID()})
		prompt := fmt.Sprintf("请直接发送每段的秒数（%d–%d），例如 119", videoSplitMinSec, videoSplitMaxSec)
		if mode == storconfig.SplitModeSize {
			prompt = fmt.Sprintf("请直接发送每段的最大大小，单位 MB（%d–%d），例如 500", videoSplitMinMB, videoSplitMaxMB)
		}
		editVideoSplit(ctx, update, videoSplitDetailText(st, prompt+"\n5 分钟内有效。"), &tg.ReplyInlineMarkup{Rows: []tg.KeyboardInlineButtonRow{
			{Buttons: []tg.KeyboardInlineButton{markup.Callback("取消", vsplitData("s", id))}},
		}})
		return dispatcher.EndGroups
	default:
		return alert("无效的按钮数据")
	}
}

// handleVideoSplitInput consumes the next plain text after「设置秒数/大小」.
func handleVideoSplitInput(ctx *ext.Context, update *ext.Update) error {
	msg := update.EffectiveMessage
	if msg == nil || strings.HasPrefix(strings.TrimSpace(msg.Text), "/") {
		return dispatcher.ContinueGroups
	}
	userID := responsibleUserID(update)
	chat := chatID(update)
	pending, ok := videoSplitInputs.take(userID, chat)
	if !ok {
		return dispatcher.ContinueGroups
	}
	st := userTelegramStorage(userID, pending.storageID)
	if st == nil {
		ctx.Reply(update, ext.ReplyTextString("你没有这个存储的权限"), nil)
		return dispatcher.EndGroups
	}
	value, err := parseVideoSplitValue(pending.mode, msg.Text)
	if err != nil {
		videoSplitInputs.set(userID, chat, pending)
		ctx.Reply(update, ext.ReplyTextString(err.Error()+"，请重新发送，或点「取消」。"), nil)
		return dispatcher.EndGroups
	}
	patch := map[string]any{"split_mode": pending.mode}
	note := fmt.Sprintf("已设置为每段 %d 秒", value)
	if pending.mode == storconfig.SplitModeSize {
		patch["split_size_mb"] = value
		note = fmt.Sprintf("已设置为每段不超过 %d MB", value)
	} else {
		patch["split_duration_sec"] = value
	}
	if err := updateStorageConfig(ctx, st.GetID(), patch); err != nil {
		ctx.Reply(update, ext.ReplyTextString("保存失败："+err.Error()), nil)
		return dispatcher.EndGroups
	}
	if fresh := userTelegramStorage(userID, st.GetID()); fresh != nil {
		st = fresh
	}
	ctx.Reply(update, ext.ReplyTextString(videoSplitDetailText(st, note)), &ext.ReplyOpts{Markup: videoSplitDetailMarkup(st)})
	return dispatcher.EndGroups
}
