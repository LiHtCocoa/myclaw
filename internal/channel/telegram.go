package channel

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cexll/agentsdk-go/pkg/api"
	"github.com/cexll/agentsdk-go/pkg/model"
	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
	"github.com/stellarlinkco/myclaw/internal/bus"
	"github.com/stellarlinkco/myclaw/internal/config"
)

const telegramChannelName = "telegram"

type TelegramChannel struct {
	BaseChannel
	token      string
	bot        *telego.Bot
	proxy      string
	httpClient *http.Client
	cancel     context.CancelFunc
	feedback   string // "debug", "normal", "minimal", "silent"
	streaming  bool
	workspace  string // workspace root for file saving

	// Media group buffering: Telegram sends multi-photo messages as separate
	// Message objects with the same MediaGroupID. We collect them briefly
	// before merging into a single dispatch.
	mgMu     sync.Mutex
	mgBuffer map[string]*mediaGroup
}

func NewTelegramChannel(cfg config.TelegramConfig, b *bus.MessageBus) (*TelegramChannel, error) {
	if cfg.Token == "" {
		return nil, fmt.Errorf("telegram token is required")
	}
	feedback := cfg.Feedback
	if feedback == "" {
		feedback = "normal"
	}
	ch := &TelegramChannel{
		BaseChannel: NewBaseChannel(telegramChannelName, b, cfg.AllowFrom),
		token:       cfg.Token,
		proxy:       cfg.Proxy,
		httpClient:  http.DefaultClient,
		feedback:    feedback,
		streaming:   cfg.Streaming,
	}
	return ch, nil
}

// SetWorkspace sets the workspace directory for file saving.
func (t *TelegramChannel) SetWorkspace(dir string) { t.workspace = dir }

func (t *TelegramChannel) initBot() error {
	var opts []telego.BotOption

	var client *http.Client
	if t.proxy != "" {
		proxyURL, err := url.Parse(t.proxy)
		if err != nil {
			return fmt.Errorf("parse proxy url: %w", err)
		}
		client = &http.Client{
			Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
		}
	} else {
		client = http.DefaultClient
	}
	t.httpClient = client
	opts = append(opts, telego.WithHTTPClient(client))

	bot, err := telego.NewBot(t.token, opts...)
	if err != nil {
		return fmt.Errorf("create telegram bot: %w", err)
	}
	t.bot = bot

	me, err := bot.GetMe(context.Background())
	if err != nil {
		return fmt.Errorf("telegram getMe: %w", err)
	}
	log.Printf("[telegram] authorized as @%s", me.Username)
	return nil
}

func (t *TelegramChannel) Start(ctx context.Context) error {
	if err := t.initBot(); err != nil {
		return err
	}

	ctx, t.cancel = context.WithCancel(ctx)

	updates, err := t.bot.UpdatesViaLongPolling(ctx, &telego.GetUpdatesParams{Timeout: 30})
	if err != nil {
		return fmt.Errorf("start long polling: %w", err)
	}

	go func() {
		for update := range updates {
			if update.Message != nil {
				t.handleMessage(update.Message)
			}
		}
	}()

	log.Printf("[telegram] polling started")
	return nil
}

// mediaGroup collects messages belonging to the same Telegram media group.
type mediaGroup struct {
	msgs  []*telego.Message
	timer *time.Timer
}

func (t *TelegramChannel) handleMessage(msg *telego.Message) {
	if msg.From == nil {
		return
	}
	senderID := strconv.FormatInt(msg.From.ID, 10)
	if !t.IsAllowed(senderID) {
		log.Printf("[telegram] rejected message from %s (%s)", senderID, msg.From.Username)
		return
	}

	// Media group: buffer and merge into single dispatch.
	if msg.MediaGroupID != "" {
		t.bufferMediaGroup(msg)
		return
	}

	// Normal (non-group) message: dispatch immediately.
	t.dispatchMessage(msg)
}

// bufferMediaGroup collects messages with the same MediaGroupID.
// A short timer merges them into a single dispatch.
func (t *TelegramChannel) bufferMediaGroup(msg *telego.Message) {
	t.mgMu.Lock()
	defer t.mgMu.Unlock()
	if t.mgBuffer == nil {
		t.mgBuffer = make(map[string]*mediaGroup)
	}
	gid := msg.MediaGroupID
	g, ok := t.mgBuffer[gid]
	if !ok {
		g = &mediaGroup{}
		t.mgBuffer[gid] = g
		g.timer = time.AfterFunc(500*time.Millisecond, func() { t.flushMediaGroup(gid) })
	} else {
		g.timer.Reset(500 * time.Millisecond)
	}
	g.msgs = append(g.msgs, msg)
}

// flushMediaGroup merges all buffered messages for a media group into one dispatch.
func (t *TelegramChannel) flushMediaGroup(gid string) {
	t.mgMu.Lock()
	g, ok := t.mgBuffer[gid]
	if !ok {
		t.mgMu.Unlock()
		return
	}
	delete(t.mgBuffer, gid)
	t.mgMu.Unlock()

	if len(g.msgs) == 0 {
		return
	}
	// Use the first message as the base (carries reply context, forward origin, caption).
	primary := g.msgs[0]
	var allContent []string
	var allBlocks []model.ContentBlock

	for _, m := range g.msgs {
		c, b := t.extractContent(m)
		if c != "" {
			allContent = append(allContent, c)
		}
		allBlocks = append(allBlocks, b...)
	}

	// Deduplicate text: reply context and forward labels appear in every message
	// of the group, but we only want them once. Use the first message's full
	// content and only append unique captions from subsequent messages.
	content := allContent[0]
	if len(allContent) > 1 {
		seen := map[string]bool{content: true}
		for _, c := range allContent[1:] {
			if !seen[c] {
				seen[c] = true
				content += "\n" + c
			}
		}
	}

	chatID := strconv.FormatInt(primary.Chat.ID, 10)
	metadata := map[string]any{
		"username":   primary.From.Username,
		"first_name": primary.From.FirstName,
		"message_id": primary.MessageID,
	}
	t.bus.Inbound <- bus.InboundMessage{
		Channel:       telegramChannelName,
		SenderID:      strconv.FormatInt(primary.From.ID, 10),
		ChatID:        chatID,
		Content:       content,
		Timestamp:     time.Unix(int64(primary.Date), 0),
		ContentBlocks: allBlocks,
		Metadata:      metadata,
	}
}

// dispatchMessage extracts content from a single message and sends it to the bus.
func (t *TelegramChannel) dispatchMessage(msg *telego.Message) {
	content, blocks := t.extractContent(msg)
	if content == "" && len(blocks) == 0 {
		return
	}
	chatID := strconv.FormatInt(msg.Chat.ID, 10)
	metadata := map[string]any{
		"username":   msg.From.Username,
		"first_name": msg.From.FirstName,
		"message_id": msg.MessageID,
	}
	t.bus.Inbound <- bus.InboundMessage{
		Channel:       telegramChannelName,
		SenderID:      strconv.FormatInt(msg.From.ID, 10),
		ChatID:        chatID,
		Content:       content,
		Timestamp:     time.Unix(int64(msg.Date), 0),
		ContentBlocks: blocks,
		Metadata:      metadata,
	}
}

// extractContent extracts text content, content blocks, reply context, and forward hints from a Telegram message.
func (t *TelegramChannel) extractContent(msg *telego.Message) (string, []model.ContentBlock) {
	var parts []string
	var blocks []model.ContentBlock
	// Reply context: prepend the replied-to message as context.
	// Three scenarios: ReplyToMessage (same chat), ExternalReply (cross-chat), Quote (text snippet).
	if reply := msg.ReplyToMessage; reply != nil {
		parts = append(parts, extractReplyContext(reply))
	} else if msg.ExternalReply != nil || msg.Quote != nil {
		extCtx, extBlocks := t.extractExternalReplyContext(msg.ExternalReply, msg.Quote)
		parts = append(parts, extCtx)
		blocks = append(blocks, extBlocks...)
	}

	// Forwarded message: add origin label.
	if label := forwardOriginLabel(msg); label != "" {
		parts = append(parts, label)
	}

	// Message body text.
	body := msg.Text
	if body == "" {
		body = msg.Caption
	} else if msg.Caption != "" {
		body = body + "\n" + msg.Caption
	}

	// For standalone forwarded messages (no user text), hint the agent to summarize.
	if msg.ForwardOrigin != nil && body == "" {
		parts = append(parts, "[The user forwarded this message without comment. Summarize or process the content above.]")
	}

	if body != "" {
		parts = append(parts, body)
	}

	content := strings.Join(parts, "\n")
	// Photos: keep as image content blocks (LLMs can process these).
	if len(msg.Photo) > 0 {
		photo := msg.Photo[len(msg.Photo)-1]
		data, err := t.downloadFileData(photo.FileID)
		if err != nil {
			log.Printf("[telegram] download photo %s failed: %v", photo.FileID, err)
		} else {
			mediaType := http.DetectContentType(data)
			if mediaType == "application/octet-stream" {
				mediaType = "image/jpeg"
			}
			blocks = append(blocks, model.ContentBlock{
				Type:      model.ContentBlockImage,
				MediaType: mediaType,
				Data:      base64.StdEncoding.EncodeToString(data),
			})
		}
	}
	// Non-image files: save to workspace and pass path reference.
	if msg.Voice != nil {
		if path, err := t.saveFile(msg.Voice.FileID, "voice.ogg"); err != nil {
			log.Printf("[telegram] save voice failed: %v", err)
			content = appendLine(content, fmt.Sprintf("[Voice message, %ds, download failed]", msg.Voice.Duration))
		} else {
			content = appendLine(content, "[Voice message saved to: "+path+"]")
		}
	}
	if msg.Audio != nil {
		name := msg.Audio.FileName
		if name == "" {
			name = "audio.mp3"
		}
		if path, err := t.saveFile(msg.Audio.FileID, name); err != nil {
			log.Printf("[telegram] save audio failed: %v", err)
			content = appendLine(content, fmt.Sprintf("[Audio: %s, download failed]", name))
		} else {
			content = appendLine(content, "[Audio file saved to: "+path+"]")
		}
	}
	if msg.Video != nil {
		name := msg.Video.FileName
		if name == "" {
			name = "video.mp4"
		}
		if path, err := t.saveFile(msg.Video.FileID, name); err != nil {
			log.Printf("[telegram] save video failed: %v", err)
			content = appendLine(content, fmt.Sprintf("[Video: %s, download failed]", name))
		} else {
			content = appendLine(content, "[Video file saved to: "+path+"]")
		}
	}
	if msg.Document != nil {
		name := msg.Document.FileName
		if name == "" {
			name = "document"
		}
		mediaType := msg.Document.MimeType
		if strings.HasPrefix(mediaType, "image/") {
			data, err := t.downloadFileData(msg.Document.FileID)
			if err != nil {
				log.Printf("[telegram] download document %s failed: %v", msg.Document.FileID, err)
				content = appendLine(content, fmt.Sprintf("[Image document: %s (%s), download failed]", name, mediaType))
			} else {
				blocks = append(blocks, model.ContentBlock{
					Type:      model.ContentBlockImage,
					MediaType: mediaType,
					Data:      base64.StdEncoding.EncodeToString(data),
				})
			}
		} else {
			if path, err := t.saveFile(msg.Document.FileID, name); err != nil {
				log.Printf("[telegram] save document failed: %v", err)
				info := fmt.Sprintf("[File: %s (%s)", name, mediaType)
				if msg.Document.FileSize > 0 {
					info += fmt.Sprintf(", %d bytes", msg.Document.FileSize)
				}
				info += ", download failed]"
				content = appendLine(content, info)
			} else {
				content = appendLine(content, "[File saved to: "+path+"]")
			}
		}
	}
	return content, blocks
}

// forwardOriginLabel returns a label like "[Forwarded from UserName]" for forwarded messages.
func forwardOriginLabel(msg *telego.Message) string {
	if msg.ForwardOrigin == nil {
		return ""
	}
	switch o := msg.ForwardOrigin.(type) {
	case *telego.MessageOriginUser:
		name := strings.TrimSpace(o.SenderUser.FirstName + " " + o.SenderUser.LastName)
		return "[Forwarded from " + name + "]"
	case *telego.MessageOriginHiddenUser:
		return "[Forwarded from " + o.SenderUserName + "]"
	case *telego.MessageOriginChat:
		return "[Forwarded from chat: " + o.SenderChat.Title + "]"
	case *telego.MessageOriginChannel:
		return "[Forwarded from channel: " + o.Chat.Title + "]"
	default:
		return "[Forwarded message]"
	}
}

// extractReplyContext builds a context block from the replied-to message.
func extractReplyContext(reply *telego.Message) string {
	var b strings.Builder
	b.WriteString("[Replying to")
	if reply.From != nil {
		name := strings.TrimSpace(reply.From.FirstName + " " + reply.From.LastName)
		if name != "" {
			b.WriteString(" " + name)
		}
	}
	b.WriteString("]")
	text := reply.Text
	if text == "" {
		text = reply.Caption
	}
	if text != "" {
		b.WriteString("\n" + text)
	}
	if reply.Voice != nil {
		b.WriteString("\n[Voice message]")
	}
	if reply.Audio != nil {
		b.WriteString("\n[Audio: " + reply.Audio.FileName + "]")
	}
	if reply.Document != nil {
		b.WriteString("\n[File: " + reply.Document.FileName + "]")
	}
	if len(reply.Photo) > 0 {
		b.WriteString("\n[Photo]")
	}
	return b.String()
}

// extractExternalReplyContext builds context from cross-chat replies (ExternalReply + Quote).
// Returns text context and optional image content blocks from the external reply.
func (t *TelegramChannel) extractExternalReplyContext(ext *telego.ExternalReplyInfo, quote *telego.TextQuote) (string, []model.ContentBlock) {
	var b strings.Builder
	var blocks []model.ContentBlock
	b.WriteString("[Replying to")
	if ext != nil {
		switch o := ext.Origin.(type) {
		case *telego.MessageOriginUser:
			name := strings.TrimSpace(o.SenderUser.FirstName + " " + o.SenderUser.LastName)
			b.WriteString(" " + name)
		case *telego.MessageOriginHiddenUser:
			b.WriteString(" " + o.SenderUserName)
		case *telego.MessageOriginChat:
			b.WriteString(" chat: " + o.SenderChat.Title)
		case *telego.MessageOriginChannel:
			b.WriteString(" channel: " + o.Chat.Title)
		}
		// Photos: download and add as image content blocks.
		if len(ext.Photo) > 0 {
			photo := ext.Photo[len(ext.Photo)-1]
			data, err := t.downloadFileData(photo.FileID)
			if err != nil {
				log.Printf("[telegram] download external reply photo failed: %v", err)
				b.WriteString("\n[Photo, download failed]")
			} else {
				mediaType := http.DetectContentType(data)
				if mediaType == "application/octet-stream" {
					mediaType = "image/jpeg"
				}
				blocks = append(blocks, model.ContentBlock{
					Type:      model.ContentBlockImage,
					MediaType: mediaType,
					Data:      base64.StdEncoding.EncodeToString(data),
				})
			}
		}
		// Non-image media: text indicators.
		if ext.Voice != nil {
			b.WriteString("\n[Voice message]")
		}
		if ext.Audio != nil {
			b.WriteString("\n[Audio: " + ext.Audio.FileName + "]")
		}
		if ext.Document != nil {
			b.WriteString("\n[File: " + ext.Document.FileName + "]")
		}
		if ext.Video != nil {
			b.WriteString("\n[Video]")
		}
		if ext.Sticker != nil {
			b.WriteString("\n[Sticker: " + ext.Sticker.Emoji + "]")
		}
	}
	b.WriteString("]")
	if quote != nil && quote.Text != "" {
		b.WriteString("\n" + quote.Text)
	}
	return b.String(), blocks
}

// saveFile downloads a Telegram file and saves it to workspace/uploads/.
// Returns the absolute path of the saved file.
func (t *TelegramChannel) saveFile(fileID, name string) (string, error) {
	if t.workspace == "" {
		return "", fmt.Errorf("workspace not configured")
	}
	data, err := t.downloadFileData(fileID)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(t.workspace, "uploads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create uploads dir: %w", err)
	}
	// Use timestamp prefix to avoid collisions.
	name = fmt.Sprintf("%d_%s", time.Now().UnixMilli(), name)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write file: %w", err)
	}
	log.Printf("[telegram] saved file to %s (%d bytes)", path, len(data))
	return path, nil
}

func appendLine(s, line string) string {
	if s == "" {
		return line
	}
	return s + "\n" + line
}
func (t *TelegramChannel) downloadFileData(fileID string) ([]byte, error) {
	if t.bot == nil {
		return nil, fmt.Errorf("telegram bot not initialized")
	}
	file, err := t.bot.GetFile(context.Background(), &telego.GetFileParams{FileID: fileID})
	if err != nil {
		return nil, fmt.Errorf("get telegram file: %w", err)
	}
	downloadURL := t.bot.FileDownloadURL(file.FilePath)
	client := t.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Get(downloadURL)
	if err != nil {
		return nil, fmt.Errorf("download telegram file: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download telegram file: unexpected status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read telegram file body: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("telegram file is empty")
	}
	return data, nil
}
func (t *TelegramChannel) Stop() error {
	if t.cancel != nil {
		t.cancel()
	}
	log.Printf("[telegram] stopped")
	return nil
}

// PreProcessFeedback sends acknowledgment feedback when a message is received.
func (t *TelegramChannel) PreProcessFeedback(chatID int64, messageID int) {
	switch t.feedback {
	case "debug":
		t.sendReaction(chatID, messageID, "👀")
		t.sendTyping(chatID)
	case "normal":
		t.sendReaction(chatID, messageID, "👍")
		t.sendTyping(chatID)
	case "minimal":
		t.sendTyping(chatID)
	case "silent":
		// no feedback
	}
}

// sendReaction sends an emoji reaction to a message.
func (t *TelegramChannel) sendReaction(chatID int64, messageID int, emoji string) {
	if t.bot == nil {
		return
	}
	err := t.bot.SetMessageReaction(context.Background(), &telego.SetMessageReactionParams{
		ChatID:    tu.ID(chatID),
		MessageID: messageID,
		Reaction:  []telego.ReactionType{tu.ReactionEmoji(emoji)},
	})
	if err != nil {
		log.Printf("[telegram] sendReaction failed: %v", err)
	}
}

// sendTyping sends a typing indicator to the chat.
func (t *TelegramChannel) sendTyping(chatID int64) {
	if t.bot == nil {
		return
	}
	err := t.bot.SendChatAction(context.Background(), tu.ChatAction(tu.ID(chatID), telego.ChatActionTyping))
	if err != nil {
		log.Printf("[telegram] sendTyping failed: %v", err)
	}
}

// sendPlaceholder sends a placeholder message and returns its message ID.
func (t *TelegramChannel) sendPlaceholder(chatID int64, text, parseMode string, silent bool) (int, error) {
	if t.bot == nil {
		return 0, fmt.Errorf("telegram bot not initialized")
	}
	msg := tu.Message(tu.ID(chatID), text)
	if parseMode != "" {
		msg = msg.WithParseMode(parseMode)
	}
	if silent {
		msg = msg.WithDisableNotification()
	}
	sent, err := t.bot.SendMessage(context.Background(), msg)
	if err != nil {
		return 0, err
	}
	return sent.MessageID, nil
}

// deleteMessage deletes an existing message.
func (t *TelegramChannel) deleteMessage(chatID int64, messageID int) error {
	if t.bot == nil {
		return fmt.Errorf("telegram bot not initialized")
	}
	return t.bot.DeleteMessage(context.Background(), &telego.DeleteMessageParams{
		ChatID:    tu.ID(chatID),
		MessageID: messageID,
	})
}

// editMessage edits an existing message. Silently ignores "message is not modified" errors.
func (t *TelegramChannel) editMessage(chatID int64, messageID int, text string, parseMode string) error {
	if t.bot == nil {
		return fmt.Errorf("telegram bot not initialized")
	}
	edit := tu.EditMessageText(tu.ID(chatID), messageID, text)
	if parseMode != "" {
		edit = edit.WithParseMode(parseMode)
	}
	_, err := t.bot.EditMessageText(context.Background(), edit)
	if err != nil {
		if strings.Contains(err.Error(), "message is not modified") {
			return nil
		}
		return err
	}
	return nil
}
func (t *TelegramChannel) Send(msg bus.OutboundMessage) error {
	if t.bot == nil {
		return fmt.Errorf("telegram bot not initialized")
	}
	chatID, err := strconv.ParseInt(msg.ChatID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid chat id %q: %w", msg.ChatID, err)
	}
	// Check if we should edit a placeholder instead of sending new message
	if placeholderID, ok := msg.Metadata["placeholder_id"]; ok {
		if pid, ok := placeholderID.(int); ok && pid != 0 {
			content := toTelegramHTML(msg.Content)
			if err := t.editMessage(chatID, pid, content, telego.ModeHTML); err != nil {
				log.Printf("[telegram] edit placeholder failed: %v", err)
			} else {
				return nil
			}
		}
	}
	content := toTelegramHTML(msg.Content)
	const maxLen = 4000
	for len(content) > 0 {
		chunk := content
		if len(chunk) > maxLen {
			idx := strings.LastIndex(chunk[:maxLen], "\n")
			if idx > 0 {
				chunk = chunk[:idx]
			} else {
				chunk = chunk[:maxLen]
			}
		}
		content = content[len(chunk):]
		tgMsg := tu.Message(tu.ID(chatID), chunk).WithParseMode(telego.ModeHTML)
		if _, err := t.bot.SendMessage(context.Background(), tgMsg); err != nil {
			// Retry without HTML parse mode
			plain := tu.Message(tu.ID(chatID), msg.Content)
			if _, err2 := t.bot.SendMessage(context.Background(), plain); err2 != nil {
				return fmt.Errorf("send telegram message: %w", err2)
			}
			return nil
		}
	}
	return nil
}

// --- Status Card for streaming feedback ---

type toolStatus int

const (
	toolRunning toolStatus = iota
	toolDone
	toolError
)

type toolEntry struct {
	name    string
	summary string
	status  toolStatus
}

type statusCard struct {
	tools     []toolEntry
	started   time.Time
	iteration int
	toolIndex map[string]int // toolUseID -> index in tools
}

type streamMsg struct {
	id       int
	dirty    bool
	lastEdit time.Time
}

func newStatusCard() *statusCard {
	return &statusCard{
		started:   time.Now(),
		toolIndex: make(map[string]int),
	}
}

func (c *statusCard) addTool(toolUseID, name, summary string) {
	c.toolIndex[toolUseID] = len(c.tools)
	c.tools = append(c.tools, toolEntry{name: name, summary: summary, status: toolRunning})
}

func (c *statusCard) finishTool(toolUseID string, failed bool) {
	if idx, ok := c.toolIndex[toolUseID]; ok {
		if failed {
			c.tools[idx].status = toolError
		} else {
			c.tools[idx].status = toolDone
		}
	}
}

func (c *statusCard) render() string {
	var b strings.Builder
	b.WriteString("🤖 <b>Working...</b>\n")
	if c.iteration > 0 {
		fmt.Fprintf(&b, "\n🔄 Iteration %d\n", c.iteration)
	}
	if len(c.tools) > 0 {
		b.WriteString("\n")
		for _, t := range c.tools {
			var icon string
			switch t.status {
			case toolRunning:
				icon = "⏳"
			case toolDone:
				icon = "✅"
			case toolError:
				icon = "❌"
			}
			if t.summary != "" {
				fmt.Fprintf(&b, "%s <code>%s</code>(%s)\n", icon, escapeHTML(t.name), escapeHTML(t.summary))
			} else {
				fmt.Fprintf(&b, "%s <code>%s</code>\n", icon, escapeHTML(t.name))
			}
		}
	}
	elapsed := time.Since(c.started).Truncate(time.Second)
	fmt.Fprintf(&b, "\n⏱ %s", elapsed)
	return b.String()
}

// summarizeToolInput extracts a short description from a tool's input JSON.
func summarizeToolInput(name string, input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return ""
	}
	// Pick the most meaningful field per tool type
	for _, key := range []string{"filePath", "file_path", "path", "command", "query", "pattern", "url"} {
		if v, ok := m[key]; ok {
			s := fmt.Sprintf("%v", v)
			if len(s) > 40 {
				s = s[:37] + "..."
			}
			return s
		}
	}
	return ""
}

// SendStream implements streaming output for TelegramChannel.
func (t *TelegramChannel) SendStream(ctx context.Context, chatID string, metadata map[string]any, events <-chan api.StreamEvent) error {
	if t.bot == nil {
		return fmt.Errorf("telegram bot not initialized")
	}
	numChatID, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid chat id %q: %w", chatID, err)
	}
	// If streaming is disabled, collect all events and call Send
	if !t.streaming {
		var sb strings.Builder
		for event := range events {
			if event.Type == api.EventContentBlockDelta && event.Delta != nil {
				sb.WriteString(event.Delta.Text)
			}
		}
		result := sb.String()
		if result == "" {
			return nil
		}
		return t.Send(bus.OutboundMessage{ChatID: chatID, Content: result, Metadata: metadata})
	}
	// Streaming mode:
	// 1) status card message: tool/status progress
	// 2) content message: intermediate text deltas
	// They are updated independently and removed when final report is sent.
	var statusMsg streamMsg
	var contentMsg streamMsg
	var textBuf strings.Builder
	var streamErr string
	const (
		statusMinGap         = 500 * time.Millisecond
		contentMinGap        = 1 * time.Second
		statusHeartbeatDelay = 5 * time.Second
	)
	card := newStatusCard()
	showCard := t.feedback == "debug" || t.feedback == "normal"
	showCursor := t.feedback != "silent"

	// Accumulator for tool input JSON chunks (content_block_delta with input_json_delta)
	var pendingToolInput map[string][]byte // toolUseID -> accumulated JSON
	var blockToolID string                 // current content_block's tool_use_id

	upsertMessage := func(msg *streamMsg, text, parseMode string, silent bool, now time.Time) bool {
		if text == "" {
			return false
		}
		if msg.id == 0 {
			pid, err := t.sendPlaceholder(numChatID, text, parseMode, silent)
			if err != nil {
				log.Printf("[telegram] stream placeholder failed: %v", err)
				return false
			}
			msg.id = pid
		} else {
			if err := t.editMessage(numChatID, msg.id, text, parseMode); err != nil {
				log.Printf("[telegram] stream edit failed: %v", err)
				return false
			}
		}
		msg.lastEdit = now
		msg.dirty = false
		return true
	}

	renderContent := func() string {
		text := textBuf.String()
		if text == "" {
			return ""
		}
		if showCursor {
			text += "▍"
		}
		return toTelegramHTML(text)
	}

	// Event-driven: update status card immediately, with per-message rate limit.
	tryUpdateStatus := func(now time.Time) {
		if !showCard {
			return
		}
		if !statusMsg.lastEdit.IsZero() && now.Sub(statusMsg.lastEdit) < statusMinGap {
			statusMsg.dirty = true // deferred to ticker
			return
		}
		upsertMessage(&statusMsg, card.render(), telego.ModeHTML, true, now)
	}

	// Ticker-driven: deferred status + content + heartbeat.
	tickFlush := func(now time.Time) {
		if showCard && statusMsg.dirty && (statusMsg.lastEdit.IsZero() || now.Sub(statusMsg.lastEdit) >= statusMinGap) {
			upsertMessage(&statusMsg, card.render(), telego.ModeHTML, true, now)
		}
		if contentMsg.dirty && (contentMsg.lastEdit.IsZero() || now.Sub(contentMsg.lastEdit) >= contentMinGap) {
			upsertMessage(&contentMsg, renderContent(), telego.ModeHTML, true, now)
		}
		if showCard && statusMsg.id != 0 && !statusMsg.dirty && (statusMsg.lastEdit.IsZero() || now.Sub(statusMsg.lastEdit) >= statusHeartbeatDelay) {
			upsertMessage(&statusMsg, card.render(), telego.ModeHTML, true, now)
		}
	}

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for events != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			tickFlush(time.Now())
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if t.feedback == "debug" && event.Type != api.EventContentBlockDelta && event.Type != api.EventContentBlockStop && event.Type != api.EventPing {
				log.Printf("[telegram] stream event: type=%s name=%s", event.Type, event.Name)
			}
			switch event.Type {
			case api.EventIterationStart:
				if event.Iteration != nil {
					card.iteration = *event.Iteration + 1
				}
				if textBuf.Len() > 0 {
					textBuf.Reset()
				}
				tryUpdateStatus(time.Now())

			case api.EventContentBlockStart:
				if event.ContentBlock != nil && event.ContentBlock.Type == "tool_use" {
					blockToolID = event.ContentBlock.ID
				} else {
					blockToolID = ""
				}

			case api.EventContentBlockStop:
				blockToolID = ""

			case api.EventContentBlockDelta:
				if event.Delta == nil {
					continue
				}
				if event.Delta.Type == "input_json_delta" && blockToolID != "" {
					if pendingToolInput == nil {
						pendingToolInput = make(map[string][]byte)
					}
					var chunk string
					if json.Unmarshal(event.Delta.PartialJSON, &chunk) == nil {
						pendingToolInput[blockToolID] = append(pendingToolInput[blockToolID], []byte(chunk)...)
					}
					continue
				}
				if event.Delta.Text != "" {
					textBuf.WriteString(event.Delta.Text)
					contentMsg.dirty = true
				}

			case api.EventToolExecutionStart:
				var summary string
				if pendingToolInput != nil {
					if raw, ok := pendingToolInput[event.ToolUseID]; ok {
						summary = summarizeToolInput(event.Name, json.RawMessage(raw))
						delete(pendingToolInput, event.ToolUseID)
					}
				}
				card.addTool(event.ToolUseID, event.Name, summary)
				tryUpdateStatus(time.Now())

			case api.EventToolExecutionResult:
				failed := false
				if event.IsError != nil && *event.IsError {
					failed = true
				}
				card.finishTool(event.ToolUseID, failed)
				tryUpdateStatus(time.Now())

			case api.EventError:
				streamErr = strings.TrimSpace(fmt.Sprintf("%v", event.Output))
				log.Printf("[telegram] stream error: %s", streamErr)
				tryUpdateStatus(time.Now())
			}
		}
	}

	// Final output
	finalText := textBuf.String()
	if finalText == "" {
		if streamErr != "" {
			finalText = "stream failed: " + streamErr
		} else {
			finalText = "agent return null"
		}
	}

	// Remove intermediate status/content messages before sending final report.
	if statusMsg.id != 0 {
		if err := t.deleteMessage(numChatID, statusMsg.id); err != nil {
			log.Printf("[telegram] delete status message failed: %v", err)
		}
	}
	if contentMsg.id != 0 {
		if err := t.deleteMessage(numChatID, contentMsg.id); err != nil {
			log.Printf("[telegram] delete content message failed: %v", err)
		}
	}

	return t.Send(bus.OutboundMessage{ChatID: chatID, Content: finalText, Metadata: metadata})
}

// toTelegramHTML converts basic markdown to Telegram HTML.
func toTelegramHTML(s string) string {
	s = convertThinkTags(s)
	type segment struct {
		text   string
		isCode bool
	}
	var segments []segment
	for len(s) > 0 {
		if idx := strings.Index(s, "```"); idx >= 0 {
			if idx > 0 {
				segments = append(segments, segment{text: s[:idx]})
			}
			end := strings.Index(s[idx+3:], "```")
			if end == -1 {
				segments = append(segments, segment{text: s[idx:]})
				s = ""
				break
			}
			end += idx + 3
			code := s[idx+3 : end]
			if nl := strings.Index(code, "\n"); nl >= 0 {
				firstLine := strings.TrimSpace(code[:nl])
				if len(firstLine) > 0 && !strings.Contains(firstLine, " ") {
					code = code[nl+1:]
				}
			}
			segments = append(segments, segment{text: "<pre>" + escapeHTML(code) + "</pre>", isCode: true})
			s = s[end+3:]
			continue
		}
		if idx := strings.Index(s, "`"); idx >= 0 {
			if idx > 0 {
				segments = append(segments, segment{text: s[:idx]})
			}
			end := strings.Index(s[idx+1:], "`")
			if end == -1 {
				segments = append(segments, segment{text: s[idx:]})
				s = ""
				break
			}
			end += idx + 1
			segments = append(segments, segment{text: "<code>" + escapeHTML(s[idx+1:end]) + "</code>", isCode: true})
			s = s[end+1:]
			continue
		}
		segments = append(segments, segment{text: s})
		break
	}
	var out strings.Builder
	for _, seg := range segments {
		if seg.isCode {
			out.WriteString(seg.text)
			continue
		}
		text := escapeHTMLPreservingTags(seg.text)
		text = convertBoldItalic(text)
		out.WriteString(text)
	}
	return out.String()
}

// escapeHTML escapes &, <, > for Telegram HTML.
func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// convertBoldItalic converts **bold** and *italic* markdown to HTML tags.
func convertBoldItalic(s string) string {
	// Bold pass: **text** -> <b>text</b>
	for {
		start := strings.Index(s, "**")
		if start == -1 {
			break
		}
		end := strings.Index(s[start+2:], "**")
		if end == -1 {
			break
		}
		end += start + 2
		// Recursively convert italic inside bold content
		inner := convertItalic(s[start+2 : end])
		s = s[:start] + "<b>" + inner + "</b>" + s[end+2:]
	}
	// Italic pass on remaining text (outside bold)
	s = convertItalic(s)
	return s
}

// convertItalic converts *italic* markdown to <i> tags.
func convertItalic(s string) string {
	for {
		start := strings.Index(s, "*")
		if start == -1 {
			break
		}
		if start+1 < len(s) && s[start+1] == '*' {
			break
		}
		end := strings.Index(s[start+1:], "*")
		if end == -1 {
			break
		}
		end += start + 1
		if end+1 < len(s) && s[end+1] == '*' {
			break
		}
		s = s[:start] + "<i>" + s[start+1:end] + "</i>" + s[end+1:]
	}
	return s
}

// convertThinkTags converts <think>...</think> to Telegram expandable blockquote.
func convertThinkTags(s string) string {
	const openTag = "<think>"
	const closeTag = "</think>"
	var result strings.Builder
	for {
		start := strings.Index(s, openTag)
		if start == -1 {
			result.WriteString(s)
			break
		}
		end := strings.Index(s[start+len(openTag):], closeTag)
		if end == -1 {
			result.WriteString(s)
			break
		}
		end += start + len(openTag)
		thinkContent := s[start+len(openTag) : end]
		result.WriteString(s[:start])
		result.WriteString("<blockquote expandable>🧠 Thinking\n")
		result.WriteString(thinkContent)
		result.WriteString("\n</blockquote>")
		s = s[end+len(closeTag):]
	}
	return result.String()
}

// escapeHTMLPreservingTags escapes &, <, > but preserves blockquote tags from convertThinkTags.
func escapeHTMLPreservingTags(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "&lt;blockquote expandable&gt;", "<blockquote expandable>")
	s = strings.ReplaceAll(s, "&lt;/blockquote&gt;", "</blockquote>")
	return s
}
