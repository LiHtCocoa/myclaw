package gateway

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cexll/agentsdk-go/pkg/api"
	"github.com/cexll/agentsdk-go/pkg/message"
	"github.com/stellarlinkco/myclaw/internal/bus"
	"github.com/stellarlinkco/myclaw/internal/runtimecmd"
)

func (g *Gateway) handleBuiltinCommand(msg bus.InboundMessage) (bool, error) {
	switch metadataString(msg.Metadata, "builtin_command") {
	case "new":
		if g.sessions == nil {
			return true, nil
		}
		_, _, err := g.sessions.Rotate(msg.SessionKey())
		return true, err
	default:
		return false, nil
	}
}

func (g *Gateway) handlePostResponse(chatSessionKey string, resp *api.Response) (string, bool, error) {
	action := responseMetadata(resp, runtimecmd.MetaPostAction)
	if action == "" {
		return "", false, nil
	}

	switch action {
	case runtimecmd.PostActionCompactRotate:
		summary := strings.TrimSpace(resultOutput(resp))
		if summary == "" {
			return "", true, fmt.Errorf("compact summary is empty")
		}
		if g.sessions == nil {
			return "", true, fmt.Errorf("session router is not configured")
		}
		oldSessionID, newSessionID, err := g.sessions.Rotate(chatSessionKey)
		if err != nil {
			return "", true, err
		}
		if err := seedSessionSummary(g.cfg.Agent.Workspace, newSessionID, summary); err != nil {
			_ = g.sessions.Set(chatSessionKey, oldSessionID)
			return "", true, err
		}
		if responseMetadata(resp, runtimecmd.MetaResponse) == runtimecmd.ResponseCompactAck {
			return "✅ Conversation compacted and continued in a fresh session.", true, nil
		}
		return summary, true, nil
	default:
		return "", false, nil
	}
}

func responseMetadata(resp *api.Response, key string) string {
	if resp == nil || key == "" {
		return ""
	}
	for _, exec := range resp.CommandResults {
		if exec.Result.Metadata == nil {
			continue
		}
		if value, ok := exec.Result.Metadata[key]; ok {
			if text := strings.TrimSpace(fmt.Sprint(value)); text != "" {
				return text
			}
		}
	}
	return ""
}

func resultOutput(resp *api.Response) string {
	if resp == nil || resp.Result == nil {
		return ""
	}
	return resp.Result.Output
}

func seedSessionSummary(workspace, sessionID, summary string) error {
	workspace = strings.TrimSpace(workspace)
	sessionID = strings.TrimSpace(sessionID)
	summary = strings.TrimSpace(summary)
	if workspace == "" || sessionID == "" || summary == "" {
		return fmt.Errorf("invalid compact seed payload")
	}

	historyDir := filepath.Join(workspace, ".claude", "history")
	if err := os.MkdirAll(historyDir, 0o700); err != nil {
		return fmt.Errorf("mkdir compact history dir: %w", err)
	}

	payload := []message.Message{{
		Role:    "system",
		Content: "Previous conversation summary:\n" + summary,
	}}

	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode compact seed: %w", err)
	}

	path := filepath.Join(historyDir, sessionID+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write compact seed: %w", err)
	}
	return nil
}

func metadataString(meta map[string]any, key string) string {
	if meta == nil {
		return ""
	}
	value, ok := meta[key]
	if !ok {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}
