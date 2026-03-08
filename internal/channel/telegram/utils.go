package telegram

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mymmrac/telego"
)

func ForwardOriginLabel(msg *telego.Message) string {
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

func ExtractReplyContext(reply *telego.Message) string {
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

func AppendLine(s, line string) string {
	if s == "" {
		return line
	}
	return s + "\n" + line
}

func SummarizeToolInput(name string, input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return ""
	}
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

func ToTelegramHTML(s string) string {
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
			segments = append(segments, segment{text: "<pre>" + EscapeHTML(code) + "</pre>", isCode: true})
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
			segments = append(segments, segment{text: "<code>" + EscapeHTML(s[idx+1:end]) + "</code>", isCode: true})
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
		text := EscapeHTMLPreservingTags(seg.text)
		text = convertBoldItalic(text)
		out.WriteString(text)
	}
	return out.String()
}

func EscapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

func convertBoldItalic(s string) string {
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
		inner := convertItalic(s[start+2 : end])
		s = s[:start] + "<b>" + inner + "</b>" + s[end+2:]
	}
	s = convertItalic(s)
	return s
}

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

func EscapeHTMLPreservingTags(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "&lt;blockquote expandable&gt;", "<blockquote expandable>")
	s = strings.ReplaceAll(s, "&lt;/blockquote&gt;", "</blockquote>")
	return s
}
