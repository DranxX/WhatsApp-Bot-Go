package handler

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
	"template-go/config"
	"template-go/plugins"
	"template-go/store"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"golang.org/x/text/unicode/norm"
)

// ─── Spam / rate-limit state ─────────────────────────────────────────────────

const (
	spamGapMs   = 2000
	spamResetMs = 30_000
)

var spamRules = map[string]spamRule{
	"command": {limit: 3, warnAt: 4, banAt: 5, banMs: 5 * 60 * 1000, label: "command"},
}

type spamRule struct {
	limit  int
	warnAt int
	banAt  int
	banMs  int64
	label  string
}

type spamEntry struct {
	count    int
	warned   bool
	lastSeen int64
}

var (
	spamMu         sync.Mutex
	spamMap        = make(map[string]*spamEntry)
	groupCacheMu   sync.RWMutex
	groupCache     = make(map[types.JID]*types.GroupInfo)
	groupCacheTime = make(map[types.JID]time.Time)
)

func init() {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			spamMu.Lock()
			now := time.Now().UnixMilli()
			for k, v := range spamMap {
				if now-v.lastSeen > spamResetMs {
					delete(spamMap, k)
				}
			}
			spamMu.Unlock()
		}
	}()
}

func spamKey(phone string) string { return "spam:" + phone }

func buildSpamKeys(phone, lid string, c *plugins.Ctx) []string {
	keys := []string{spamKey(phone)}
	if lid != "" {
		keys = append(keys, spamKey("lid:"+lid))
	}
	if c.SenderJID.User != "" {
		keys = append(keys, spamKey("jid:"+c.SenderJID.User))
	}
	if c.SenderJID.Server == "lid" {
		keys = append(keys, spamKey("lidjid:"+c.SenderJID.User))
	}
	return keys
}

// ─── Blocklist cache ──────────────────────────────────────────────────────────

var (
	blocklistMu    sync.Mutex
	blocklistCache = map[string]bool{}
	blocklistTTL   int64
)

// FetchBlocklist fetches the blocklist from WhatsApp and updates the cache.
func FetchBlocklist(client *whatsmeow.Client) (map[string]bool, error) {
	bl, err := client.GetBlocklist(context.Background())
	if err != nil {
		return nil, err
	}
	blocklistMu.Lock()
	blocklistCache = make(map[string]bool, len(bl.JIDs))
	for _, jid := range bl.JIDs {
		blocklistCache[jid.User] = true
	}
	blocklistTTL = time.Now().UnixMilli()
	blocklistMu.Unlock()
	return blocklistCache, nil
}

// ClearGroupCache removes a group from the metadata cache.
func ClearGroupCache(chat types.JID) {
	groupCacheMu.Lock()
	delete(groupCache, chat)
	delete(groupCacheTime, chat)
	groupCacheMu.Unlock()
}

func mapGroupParticipantsLids(groupMeta *types.GroupInfo) {
	if groupMeta == nil {
		return
	}
	for _, p := range groupMeta.Participants {
		cleanLid := p.LID.User
		cleanPhone := p.PhoneNumber.User
		if cleanLid != "" && cleanPhone != "" {
			store.MapLidToPhone(cleanLid, cleanPhone)
		}
	}
}

// FetchGroupMetadata fetches group metadata from WhatsApp and caches it.
func FetchGroupMetadata(client *whatsmeow.Client, chat types.JID) (*types.GroupInfo, error) {
	newInfo, err := client.GetGroupInfo(context.Background(), chat)
	if err != nil {
		return nil, err
	}
	mapGroupParticipantsLids(newInfo)
	groupCacheMu.Lock()
	groupCache[chat] = newInfo
	groupCacheTime[chat] = time.Now()
	groupCacheMu.Unlock()
	return newInfo, nil
}

func isBlocked(client *whatsmeow.Client, phone string) bool {
	blocklistMu.Lock()
	now := time.Now().UnixMilli()
	if now-blocklistTTL > 60_000 {
		blocklistTTL = now
		blocklistMu.Unlock()
		go func() { _, _ = FetchBlocklist(client) }()
	} else {
		blocklistMu.Unlock()
	}
	blocklistMu.Lock()
	blocked := blocklistCache[phone]
	blocklistMu.Unlock()
	return blocked
}

func normalizeCmd(s string) string {
	t := norm.NFD.String(s)
	var b strings.Builder
	for _, r := range t {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// Handle processes a single incoming WhatsApp message event.
func Handle(client *whatsmeow.Client, evt *events.Message) {
	receivedAt := time.Now().UnixMilli()
	cfg := config.Get()

	if evt.Info.Chat.Server == "broadcast" {
		return
	}

	rawMsg := evt.Message
	if rawMsg.GetEphemeralMessage() != nil {
		rawMsg = rawMsg.GetEphemeralMessage().Message
	}
	if rawMsg.GetViewOnceMessageV2() != nil {
		rawMsg = rawMsg.GetViewOnceMessageV2().Message
	}
	if rawMsg.GetDocumentWithCaptionMessage() != nil {
		rawMsg = rawMsg.GetDocumentWithCaptionMessage().Message
	}

	chat := evt.Info.Chat
	isGroup := chat.Server == types.GroupServer
	senderJID := evt.Info.Sender
	senderPhone := senderJID.User
	senderLid := ""
	if senderJID.Server == "lid" {
		senderLid = senderJID.User
		if mappedPhone := store.GetPhoneForLid(senderJID.User); mappedPhone != "" {
			senderPhone = mappedPhone
		}
	}
	pushName := evt.Info.PushName
	text := plugins.GetMessageText(rawMsg)
	msgType := plugins.GetMessageType(rawMsg)
	quoted := plugins.ExtractQuoted(rawMsg, client.Store.ID.User)

	botPhone := ""
	if client.Store.ID != nil {
		botPhone = client.Store.ID.User
	}
	ownerPhone := cfg.Owner
	configBot := cfg.Bot

	isOwner := evt.Info.IsFromMe ||
		senderPhone == ownerPhone ||
		senderPhone == botPhone ||
		(configBot != "" && senderPhone == configBot) ||
		store.IsOwnerPhone(senderPhone, ownerPhone)

	var groupMeta *types.GroupInfo
	var isGroupAdmin, isBotGroupAdmin bool

	if isGroup {
		if isBlocked(client, senderPhone) {
			return
		}
		groupCacheMu.RLock()
		info, exists := groupCache[chat]
		cacheTime, timeExists := groupCacheTime[chat]
		groupCacheMu.RUnlock()

		if exists && timeExists && time.Since(cacheTime) < 5*time.Minute {
			groupMeta = info
		} else if !exists {
			go func() { _, _ = FetchGroupMetadata(client, chat) }()
		} else {
			groupMeta = info
			go func() { _, _ = FetchGroupMetadata(client, chat) }()
		}

		if groupMeta != nil {
			for _, p := range groupMeta.Participants {
				if p.IsAdmin || p.IsSuperAdmin {
					if p.JID.User == senderPhone {
						isGroupAdmin = true
					}
					if client.Store.ID != nil && p.JID.User == client.Store.ID.User {
						isBotGroupAdmin = true
					}
				}
			}
		}
	}

	c := &plugins.Ctx{
		Client:          client,
		Event:           evt,
		Chat:            chat,
		SenderPhone:     senderPhone,
		SenderJID:       senderJID,
		IsGroup:         isGroup,
		IsFromMe:        evt.Info.IsFromMe,
		PushName:        pushName,
		Text:            text,
		MsgType:         msgType,
		Quoted:          quoted,
		Timestamp:       evt.Info.Timestamp,
		ReceivedAt:      receivedAt,
		Prefix:          cfg.Prefix,
		IsOwner:         isOwner,
		OwnerPhone:      ownerPhone,
		BotPhone:        botPhone,
		GroupMeta:       groupMeta,
		IsGroupAdmin:    isGroupAdmin,
		IsBotGroupAdmin: isBotGroupAdmin,
	}

	if ci := plugins.GetContextInfo(rawMsg); ci != nil {
		c.MentionedJIDs = ci.MentionedJID
	}

	// Log: [tag] preview | sender
	if msgType == "reactionMessage" || msgType == "stickerMessage" || msgType == "protocolMessage" {
		return
	}
	tag := "msg"
	tags := map[string]string{
		"conversation": "msg", "extendedTextMessage": "msg",
		"imageMessage": "img", "videoMessage": "vid",
		"stickerMessage": "sticker", "reactionMessage": "react",
		"audioMessage": "audio", "documentMessage": "doc",
		"documentWithCaptionMessage": "doc",
	}
	if evt.Info.IsFromMe {
		tag = "self"
	} else if t, ok := tags[msgType]; ok {
		tag = t
	}
	preview := text
	if len(preview) > 80 {
		preview = preview[:80]
	}
	fmt.Printf("[%s] %s | %s\n", tag, preview, pushName)

	switch cfg.Status {
	case "ponly":
		if isGroup {
			return
		}
	case "gonly":
		if !isGroup {
			return
		}
	case "self":
		if senderPhone != botPhone && senderPhone != ownerPhone {
			return
		}
	}

	for _, p := range plugins.All() {
		if p.Before != nil {
			_ = p.Before(context.Background(), c)
		}
	}

	prefix := cfg.Prefix
	isCmd := strings.HasPrefix(text, prefix)

	if !isCmd && !strings.HasPrefix(text, "$ ") {
		return
	}

	var cmdName, q string
	var args []string

	switch {
	case strings.HasPrefix(text, "$ "):
		cmdName = "$"
		q = strings.TrimPrefix(text, "$ ")
		args = strings.Fields(q)

	case isCmd:
		body := strings.TrimSpace(text[len(prefix):])
		if body != "" {
			parts := strings.Fields(body)
			cmdName = normalizeCmd(parts[0])
			args = parts[1:]
			q = strings.Join(args, " ")
		}
	}

	if cmdName == "" {
		return
	}

	plugin := plugins.Get(cmdName)
	if plugin == nil {
		return
	}

	if !isOwner && store.IsBanned(senderPhone, senderLid) {
		return
	}

	if !isOwner {
		if blocked := checkSpam(c, cmdName); blocked {
			return
		}
	}

	c.CommandName = cmdName
	c.Args = args
	c.Q = q

	fmt.Printf("[%s] %s%s %s | %s\n", cmdName, prefix, cmdName, strings.Join(args, " "), pushName)

	if cfg.Autoread == "enable" {
		go func() {
			_ = client.MarkRead(context.Background(), []types.MessageID{types.MessageID(evt.Info.ID)}, time.Now(), chat, senderJID)
		}()
	}

	if err := plugin.Handler(context.Background(), c); err != nil {
		fmt.Printf("[PLUGIN ERROR] %s: %v\n", cmdName, err)
		_ = c.Replyf("Error: %v", err)
	}
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func checkSpam(c *plugins.Ctx, cmdName string) bool {
	rule := spamRules["command"]
	now := time.Now().UnixMilli()

	lid := ""
	if c.SenderJID.Server == "lid" {
		lid = c.SenderJID.User
	}
	keys := buildSpamKeys(c.SenderPhone, lid, c)

	spamMu.Lock()
	var state *spamEntry
	for _, k := range keys {
		if s, ok := spamMap[k]; ok {
			state = s
			break
		}
	}
	if state == nil {
		state = &spamEntry{}
	}

	if now-state.lastSeen > spamGapMs {
		state.count = 0
		state.warned = false
	}
	state.count++
	state.lastSeen = now

	for _, k := range keys {
		spamMap[k] = state
	}
	count := state.count
	warned := state.warned
	spamMu.Unlock()

	if count <= rule.limit {
		return false
	}
	if count >= rule.banAt {
		store.AddBan(c.SenderPhone, lid, "spam "+rule.label, rule.banMs)
		_ = c.Replyf("You've been temporarily banned for spamming %s.", rule.label)
		spamMu.Lock()
		for _, k := range keys {
			delete(spamMap, k)
		}
		spamMu.Unlock()
		return true
	}
	if count >= rule.warnAt && !warned {
		state.warned = true
		spamMu.Unlock()
		_ = c.Replyf("Please don't spam %s. Min %ds gap. Continued spam will result in a %s ban.",
			rule.label, spamGapMs/1000, plugins.FormatDuration(rule.banMs/1000))
		return true
	}
	return true
}

