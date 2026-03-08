package runtimecmd

import (
	"context"
	"strings"

	"github.com/cexll/agentsdk-go/pkg/api"
	"github.com/cexll/agentsdk-go/pkg/runtime/commands"
)

const (
	MetaPostAction = "myclaw.post_action"
	MetaResponse   = "myclaw.response_mode"

	PostActionCompactRotate = "compact_rotate"
	ResponseCompactAck      = "compact_ack"
)

func Build() []api.CommandRegistration {
	return []api.CommandRegistration{{
		Definition: commands.Definition{
			Name:        "compact",
			Description: "Compress the current conversation into a fresh continuation context.",
		},
		Handler: commands.HandlerFunc(handleCompact),
	}}
}

func handleCompact(_ context.Context, inv commands.Invocation) (commands.Result, error) {
	focus := strings.TrimSpace(strings.Join(inv.Args, " "))
	var prompt strings.Builder
	prompt.WriteString("Create a compact continuation summary for this conversation. ")
	prompt.WriteString("This output is for the assistant, not for the end user. ")
	prompt.WriteString("Write only the summary text. Capture goals, decisions, constraints, preferences, important file paths, open questions, and the best next steps. ")
	prompt.WriteString("Do not address the user, do not mention compacting, and do not include markdown fences.")
	if focus != "" {
		prompt.WriteString(" Give extra attention to: ")
		prompt.WriteString(focus)
		prompt.WriteString(".")
	}

	return commands.Result{
		Metadata: map[string]any{
			"api.prepend_prompt": prompt.String(),
			MetaPostAction:       PostActionCompactRotate,
			MetaResponse:         ResponseCompactAck,
		},
	}, nil
}
