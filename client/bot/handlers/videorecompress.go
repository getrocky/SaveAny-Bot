package handlers

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/gotd/td/telegram/message/markup"
	"github.com/gotd/td/tg"
	"github.com/krau/SaveAny-Bot/client/bot/handlers/utils/msgelem"
	storconfig "github.com/krau/SaveAny-Bot/config/storage"
	"github.com/krau/SaveAny-Bot/pkg/tcbdata"
	"github.com/krau/mygotg/dispatcher"
	"github.com/krau/mygotg/ext"
)

// Telegram「视频重压」menu: per Telegram storage, toggle recompression of large
// videos, pick a quality level and the trigger size. Saved through the same
// storage settings as the web admin.

const (
	// videoRecompressInputMode marks a pending trigger-size input.
	videoRecompressInputMode = "recompress"

	videoRecompressMinMB = 10
	videoRecompressMaxMB = 4000
)

type videoRecompressQuality struct {
	CRF   int
	Label string
}

var videoRecompressQualities = []videoRecompressQuality{
	{CRF: 20, Label: "高画质"},
	{CRF: 23, Label: "均衡"},
	{CRF: 26, Label: "省空间"},
}

func videoRecompressQualityLabel(crf int) string {
	for _, q := range videoRecompressQualities {
		if q.CRF == crf {
			return fmt.Sprintf("%s（CRF %d）", q.Label, crf)
		}
	}
	return fmt.Sprintf("CRF %d", crf)
}

// parseVideoRecompressMB validates a trigger size in MB typed by the user.
func parseVideoRecompressMB(text string) (int64, error) {
	text = strings.TrimSpace(strings.ToLower(text))
	text = strings.TrimSpace(strings.TrimSuffix(strings.TrimSuffix(text, "mb"), "m"))
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, errors.New("请输入整数")
	}
	if value < videoRecompressMinMB || value > videoRecompressMaxMB {
		return 0, fmt.Errorf("大小需在 %d–%d MB 之间", videoRecompressMinMB, videoRecompressMaxMB)
	}
	return value, nil
}

func videoRecompressDetailText(st *storconfig.TelegramStorageConfig, extra string) string {
	state := "已关闭"
	if st.RecompressVideo {
		state = "已开启"
	}
	text := fmt.Sprintf("🗜 视频重压 · %s\n\n状态：%s\n画质：%s\n触发：大于 %d MB 的视频\n\n用 H.264 重新压缩，画质肉眼基本无损；压完小不了 10%% 就保留原视频。同时开了切片时，切片和重压一次完成。",
		st.GetName(), state, videoRecompressQualityLabel(st.EffectiveRecompressCRF()), st.EffectiveRecompressMinMB())
	if extra != "" {
		text += "\n\n" + extra
	}
	return text
}

func vrecompData(parts ...any) []byte {
	args := []string{tcbdata.TypeConfig, "vrecomp"}
	for _, p := range parts {
		args = append(args, fmt.Sprint(p))
	}
	return []byte(strings.Join(args, " "))
}

func videoRecompressDetailMarkup(st *storconfig.TelegramStorageConfig) *tg.ReplyInlineMarkup {
	id := st.GetID()
	toggle := "✅ 开启重压"
	if st.RecompressVideo {
		toggle = "⏸ 关闭重压"
	}
	current := st.EffectiveRecompressCRF()
	quality := make([]tg.KeyboardInlineButton, 0, len(videoRecompressQualities))
	for _, q := range videoRecompressQualities {
		label := q.Label
		if q.CRF == current {
			label = "• " + label
		}
		quality = append(quality, markup.Callback(label, vrecompData("q", id, q.CRF)))
	}
	return &tg.ReplyInlineMarkup{Rows: []tg.KeyboardInlineButtonRow{
		{Buttons: []tg.KeyboardInlineButton{markup.Callback(toggle, vrecompData("t", id))}},
		{Buttons: quality},
		{Buttons: []tg.KeyboardInlineButton{markup.Callback("✏️ 设置触发大小(MB)", vrecompData("i", id))}},
		{Buttons: []tg.KeyboardInlineButton{
			markup.Callback("⬅️ 返回存储列表", vrecompData()),
			markup.Callback("🏠 设置主菜单", []byte(tcbdata.TypeConfig+" menu")),
		}},
	}}
}

func videoRecompressListMarkup(storages []*storconfig.TelegramStorageConfig) *tg.ReplyInlineMarkup {
	rows := make([]tg.KeyboardInlineButtonRow, 0, len(storages)+1)
	for _, st := range storages {
		state := "关"
		if st.RecompressVideo {
			state = fmt.Sprintf("开 · >%dMB", st.EffectiveRecompressMinMB())
		}
		rows = append(rows, tg.KeyboardInlineButtonRow{Buttons: []tg.KeyboardInlineButton{
			markup.Callback(fmt.Sprintf("%s（%s）", st.GetName(), state), vrecompData("s", st.GetID())),
		}})
	}
	rows = append(rows, tg.KeyboardInlineButtonRow{Buttons: []tg.KeyboardInlineButton{
		markup.Callback("⬅️ 返回", []byte(tcbdata.TypeConfig+" menu")),
	}})
	return &tg.ReplyInlineMarkup{Rows: rows}
}

func handleConfigVideoRecompressCallback(ctx *ext.Context, update *ext.Update) error {
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
		editVideoSplit(ctx, update, "🗜 视频重压\n\n选择要设置的 Telegram 存储：", videoRecompressListMarkup(storages))
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
		editVideoSplit(ctx, update, videoRecompressDetailText(st, note), videoRecompressDetailMarkup(st))
		return dispatcher.EndGroups
	}

	switch args[2] {
	case "s":
		videoSplitInputs.clear(userID, chatID(update))
		editVideoSplit(ctx, update, videoRecompressDetailText(st, ""), videoRecompressDetailMarkup(st))
		return dispatcher.EndGroups
	case "t":
		next := !st.RecompressVideo
		note := "已关闭视频重压"
		if next {
			note = "已开启视频重压"
		}
		return save(map[string]any{"recompress_video": next}, note)
	case "q":
		if len(args) < 5 {
			return alert("无效的画质")
		}
		crf, err := strconv.Atoi(args[4])
		if err != nil || crf < 1 || crf > 51 {
			return alert("无效的画质")
		}
		return save(map[string]any{"recompress_crf": crf}, "画质已改为"+videoRecompressQualityLabel(crf))
	case "i":
		videoSplitInputs.set(userID, chatID(update), videoSplitPending{storageID: id, mode: videoRecompressInputMode, msgID: update.CallbackQuery.GetMsgID()})
		prompt := fmt.Sprintf("请直接发送触发大小，单位 MB（%d–%d），例如 200。大于这个大小的视频才会重压。", videoRecompressMinMB, videoRecompressMaxMB)
		editVideoSplit(ctx, update, videoRecompressDetailText(st, prompt+"\n5 分钟内有效。"), &tg.ReplyInlineMarkup{Rows: []tg.KeyboardInlineButtonRow{
			{Buttons: []tg.KeyboardInlineButton{markup.Callback("取消", vrecompData("s", id))}},
		}})
		return dispatcher.EndGroups
	default:
		return alert("无效的按钮数据")
	}
}
