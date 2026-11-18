package handlers

import (
	"sort"
	"sync"
	"time"

	"github.com/charmbracelet/log"
	"github.com/gotd/td/tg"
	"github.com/krau/SaveAny-Bot/client/bot/handlers/utils/mediautil"
	"github.com/krau/SaveAny-Bot/client/bot/handlers/utils/msgelem"
	"github.com/krau/SaveAny-Bot/client/bot/handlers/utils/shortcut"
	"github.com/krau/SaveAny-Bot/common/i18n"
	"github.com/krau/SaveAny-Bot/common/i18n/i18nk"
	"github.com/krau/SaveAny-Bot/common/mediafilter"
	"github.com/krau/SaveAny-Bot/common/utils/tgutil"
	"github.com/krau/SaveAny-Bot/config"
	"github.com/krau/SaveAny-Bot/database"
	"github.com/krau/SaveAny-Bot/pkg/tcbdata"
	"github.com/krau/SaveAny-Bot/pkg/tfile"
	"github.com/krau/SaveAny-Bot/storage"
	"github.com/krau/mygotg/dispatcher"
	"github.com/krau/mygotg/ext"
)

// mediaGroupKey uniquely identifies a media group by chat, sender, and group
// ID so files from different users in the same chat can never be mixed.
type mediaGroupKey struct {
	chatID  int64
	userID  int64
	groupID int64
}

type MediaGroupHandler struct {
	groups map[mediaGroupKey][]tfile.TGFileMessage
	timers map[mediaGroupKey]*time.Timer
	// dispatched remembers which message IDs of a group were already handed
	// to a task, so an album item whose update arrives after the group was
	// dispatched (slow update delivery, a backlog after a stall) is not saved
	// and returned a second time as its own single message.
	dispatched map[mediaGroupKey]dispatchedGroup
	mu         sync.Mutex
	timeout    time.Duration
	setupOnce  sync.Once
}

type dispatchedGroup struct {
	ids     map[int]struct{}
	expires time.Time
}

const (
	// defaultMediaGroupTimeout is the quiet window used when the setting is 0.
	// One second was too short: album updates can arrive more than a second
	// apart, which split one source album into several returned messages.
	defaultMediaGroupTimeout = 3 * time.Second
	dispatchedGroupTTL       = 10 * time.Minute
)

func mediaGroupTimeout(timeoutSec int) time.Duration {
	if timeoutSec < 1 {
		return defaultMediaGroupTimeout
	}
	return time.Duration(timeoutSec) * time.Second
}

func (m *MediaGroupHandler) SetupTimeout(timeoutSec int) {
	m.setupOnce.Do(func() {
		m.timeout = mediaGroupTimeout(timeoutSec)
	})
}

var (
	mediaGroupHandler = &MediaGroupHandler{
		groups:     make(map[mediaGroupKey][]tfile.TGFileMessage),
		timers:     make(map[mediaGroupKey]*time.Timer),
		dispatched: make(map[mediaGroupKey]dispatchedGroup),
		mu:         sync.Mutex{},
	}
)

// alreadyDispatched reports whether msgID was already handed to a task as part
// of the group. The caller must hold m.mu. Expired records are pruned.
func (m *MediaGroupHandler) alreadyDispatched(key mediaGroupKey, msgID int, now time.Time) bool {
	for k, d := range m.dispatched {
		if now.After(d.expires) {
			delete(m.dispatched, k)
		}
	}
	d, ok := m.dispatched[key]
	if !ok {
		return false
	}
	_, ok = d.ids[msgID]
	return ok
}

// markDispatched records the message IDs handed to a task. The caller must hold m.mu.
func (m *MediaGroupHandler) markDispatched(key mediaGroupKey, items []tfile.TGFileMessage, now time.Time) {
	d, ok := m.dispatched[key]
	if !ok {
		d = dispatchedGroup{ids: make(map[int]struct{}, len(items))}
	}
	for _, item := range items {
		if msg := item.Message(); msg != nil {
			d.ids[msg.GetID()] = struct{}{}
		}
	}
	d.expires = now.Add(dispatchedGroupTTL)
	m.dispatched[key] = d
}

func messageID(file tfile.TGFileMessage) int {
	if msg := file.Message(); msg != nil {
		return msg.GetID()
	}
	return 0
}

// sortGroupItems keeps album items in their source order.
func sortGroupItems(items []tfile.TGFileMessage) {
	sort.SliceStable(items, func(i, j int) bool { return messageID(items[i]) < messageID(items[j]) })
}

// mergeGroupItems appends fetched items whose message IDs are not yet present
// and returns the result in source order.
func mergeGroupItems(items, fetched []tfile.TGFileMessage) []tfile.TGFileMessage {
	seen := make(map[int]struct{}, len(items))
	for _, item := range items {
		seen[messageID(item)] = struct{}{}
	}
	for _, f := range fetched {
		id := messageID(f)
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		items = append(items, f)
	}
	sortGroupItems(items)
	return items
}

// fetchMissingGroupItems asks Telegram for the whole source album so items
// whose updates have not arrived yet are saved with the rest of the group
// instead of being returned later as separate messages.
func fetchMissingGroupItems(ctx *ext.Context, key mediaGroupKey, items []tfile.TGFileMessage) []tfile.TGFileMessage {
	logger := log.FromContext(ctx)
	if len(items) == 0 || items[0].Message() == nil {
		return nil
	}
	msgs, err := tgutil.GetGroupedMessages(ctx, key.chatID, items[0].Message())
	if err != nil {
		logger.Debugf("Failed to fetch full media group %d: %s", key.groupID, err)
		return nil
	}
	userDB, err := database.GetUserByChatID(ctx, key.userID)
	if err != nil {
		logger.Debugf("Failed to load user for media group %d: %s", key.groupID, err)
		return nil
	}
	have := make(map[int]struct{}, len(items))
	for _, item := range items {
		have[messageID(item)] = struct{}{}
	}
	fetched := make([]tfile.TGFileMessage, 0)
	for _, m := range msgs {
		if m == nil || m.Out {
			continue
		}
		if _, ok := have[m.GetID()]; ok {
			continue
		}
		if from, ok := m.GetFromID(); ok {
			if peer, isUser := from.(*tg.PeerUser); isUser && peer.UserID != key.userID {
				continue
			}
		}
		if m.Media == nil || !mediautil.IsSupported(m.Media) || !mediafilter.Allowed(m.Media) {
			continue
		}
		file, err := tfile.FromMediaMessage(m.Media, ctx.Raw, m, mediautil.TfileOptions(ctx, userDB, m)...)
		if err != nil {
			logger.Debugf("Failed to get file from fetched group message %d: %s", m.GetID(), err)
			continue
		}
		fetched = append(fetched, file)
	}
	return fetched
}

func handleGroupMediaMessage(ctx *ext.Context, update *ext.Update, message *tg.Message, groupID int64) error {
	mediaGroupHandler.SetupTimeout(config.C().Telegram.MediaGroupTimeout)
	logger := log.FromContext(ctx)
	media := message.Media
	if !mediautil.IsSupported(media) || !mediafilter.Allowed(media) {
		return dispatcher.EndGroups
	}
	userId := responsibleUserID(update)
	userDB, err := database.GetUserByChatID(ctx, userId)
	if err != nil {
		return err
	}
	tfOpts := mediautil.TfileOptions(ctx, userDB, message)
	file, err := tfile.FromMediaMessage(media, ctx.Raw, message, tfOpts...)
	if err != nil {
		logger.Errorf("Failed to get file from media: %s", err)
		return dispatcher.EndGroups
	}
	mediaGroupHandler.mu.Lock()
	defer mediaGroupHandler.mu.Unlock()
	key := mediaGroupKey{
		chatID:  update.EffectiveChat().GetID(),
		userID:  userId,
		groupID: groupID,
	}
	if mediaGroupHandler.alreadyDispatched(key, message.GetID(), time.Now()) {
		logger.Debugf("Media group %d item %d was already saved with its group, skipping", groupID, message.GetID())
		return dispatcher.EndGroups
	}
	if mediaGroupHandler.groups[key] == nil {
		mediaGroupHandler.groups[key] = make([]tfile.TGFileMessage, 0)
	}
	mediaGroupHandler.groups[key] = append(mediaGroupHandler.groups[key], file)

	if timer, exists := mediaGroupHandler.timers[key]; exists {
		timer.Stop()
	}
	mediaGroupHandler.timers[key] = time.AfterFunc(mediaGroupHandler.timeout, func() {
		processMediaGroup(ctx, update, key)
	})
	return dispatcher.EndGroups
}

func processMediaGroup(ctx *ext.Context, update *ext.Update, key mediaGroupKey) {
	logger := log.FromContext(ctx)
	mediaGroupHandler.mu.Lock()
	items := mediaGroupHandler.groups[key]
	delete(mediaGroupHandler.groups, key)
	delete(mediaGroupHandler.timers, key)
	mediaGroupHandler.mu.Unlock()
	if len(items) == 0 {
		logger.Warn("No media items to process for group", "groupID", key.groupID)
		return
	}
	if fetched := fetchMissingGroupItems(ctx, key, items); len(fetched) > 0 {
		logger.Debugf("Media group %d: added %d item(s) whose updates had not arrived yet", key.groupID, len(fetched))
		items = mergeGroupItems(items, fetched)
	} else {
		sortGroupItems(items)
	}
	mediaGroupHandler.mu.Lock()
	// Items that arrived while the group was being fetched belong to it too.
	if late := mediaGroupHandler.groups[key]; len(late) > 0 {
		if timer, ok := mediaGroupHandler.timers[key]; ok {
			timer.Stop()
		}
		delete(mediaGroupHandler.groups, key)
		delete(mediaGroupHandler.timers, key)
		items = mergeGroupItems(items, late)
	}
	mediaGroupHandler.markDispatched(key, items, time.Now())
	mediaGroupHandler.mu.Unlock()
	logger.Debugf("Processing media group %d with %d items", key.groupID, len(items))

	userId := responsibleUserID(update)
	msg, err := ctx.Reply(update, ext.ReplyTextString(i18n.T(i18nk.BotMsgMediaGroupInfoSavingFiles, nil)), nil)
	if err != nil {
		logger.Errorf("Failed to reply: %s", err)
		return
	}
	stor := storage.FromContext(ctx)
	if stor != nil {
		// In silent mode
		if len(items) == 1 {
			shortcut.CreateAndAddTGFileTaskWithEdit(ctx, userId, key.chatID, stor, "", items[0], msg.ID)
			return
		}
		shortcut.CreateAndAddBatchTGFileTaskWithEdit(ctx, userId, key.chatID, stor, "", items, msg.ID)
		return
	}

	stors := storage.GetUserStorages(ctx, userId)
	markup, err := msgelem.BuildAddSelectStorageKeyboard(stors, tcbdata.Add{
		Files:   items,
		AsBatch: len(items) > 1,
	})
	if err != nil {
		logger.Errorf("Failed to build storage selection keyboard: %s", err)
		ctx.EditMessage(key.chatID, &tg.MessagesEditMessageRequest{
			ID: msg.ID,
			Message: i18n.T(i18nk.BotMsgMediaGroupErrorBuildStorageSelectKeyboardFailed, map[string]any{
				"Error": err.Error(),
			}),
		})
		return
	}
	ctx.EditMessage(key.chatID, &tg.MessagesEditMessageRequest{
		ID: msg.ID,
		Message: i18n.T(i18nk.BotMsgMediaGroupInfoGroupFoundFilesSelectStorage, map[string]any{
			"Count": len(items),
		}),
		ReplyMarkup: markup,
	})
}
