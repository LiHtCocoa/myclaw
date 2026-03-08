package telegram

import (
	"fmt"
	"strings"
	"time"
)

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

type StatusCard struct {
	started   time.Time
	iteration int
	tools     []toolEntry
	toolIndex map[string]int
}

func NewStatusCard() *StatusCard {
	return &StatusCard{
		started:   time.Now(),
		toolIndex: make(map[string]int),
	}
}

func (c *StatusCard) AddTool(toolUseID, name, summary string) {
	c.toolIndex[toolUseID] = len(c.tools)
	c.tools = append(c.tools, toolEntry{name: name, summary: summary, status: toolRunning})
}

func (c *StatusCard) FinishTool(toolUseID string, failed bool) {
	if idx, ok := c.toolIndex[toolUseID]; ok {
		if failed {
			c.tools[idx].status = toolError
		} else {
			c.tools[idx].status = toolDone
		}
	}
}

func (c *StatusCard) SetIteration(n int) {
	c.iteration = n
}

func (c *StatusCard) Render() string {
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
				fmt.Fprintf(&b, "%s <code>%s</code>(%s)\n", icon, EscapeHTML(t.name), EscapeHTML(t.summary))
			} else {
				fmt.Fprintf(&b, "%s <code>%s</code>\n", icon, EscapeHTML(t.name))
			}
		}
	}
	elapsed := time.Since(c.started).Truncate(time.Second)
	fmt.Fprintf(&b, "\n⏱ %s", elapsed)
	return b.String()
}
