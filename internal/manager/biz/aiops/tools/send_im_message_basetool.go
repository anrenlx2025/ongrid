package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/ongridio/ongrid/internal/manager/biz/aiops/tools/basetool"
)

// ToolNameSendNotification is the assistant-facing wire name. It sends a
// one-way notification through a configured notification channel.
const ToolNameSendNotification = "send_notification"

// ToolNameSendIMMessage is the former wire name. It is accepted only when
// executing a workflow saved by an older release; new assistants and new
// workflow definitions must use the notification semantics instead.
const ToolNameSendIMMessage = "send_im_message"

// NotificationChannel is one configured outbound notification channel,
// narrowed to what the tool needs to resolve + report it.
type NotificationChannel struct {
	ID   uint64
	Name string
	Kind string
}

// NotificationSender is the seam to the notification-channel store + notify router. Implemented in
// cmd/main.go over the alert channel repo + notify.Router (same
// BuildSenderFromChannel path the alert notifier / flow notify node use), so
// this package stays decoupled from the data layer.
type NotificationSender interface {
	ListNotificationChannels(ctx context.Context) ([]NotificationChannel, error)
	SendNotification(ctx context.Context, channelID uint64, title, text string) error
}

// SendNotificationTool lets the assistant proactively push a message through
// a configured notification channel. It does not target a two-way IM session.
type SendNotificationTool struct {
	sender NotificationSender
	log    *slog.Logger
}

// NewSendNotificationTool builds the assistant-facing notification tool.
func NewSendNotificationTool(s NotificationSender, log *slog.Logger) *SendNotificationTool {
	if log == nil {
		log = slog.Default()
	}
	return &SendNotificationTool{sender: s, log: log}
}

var sendNotificationSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "channel": { "type": "string", "description": "目标通知渠道名——设置→通知中配置的飞书 / 钉钉 / Slack / Telegram 等渠道名称；不是双向 IM 机器人。" },
    "text": { "type": "string", "description": "要发送的正文（纯文本，可带换行）。" },
    "title": { "type": "string", "description": "可选标题 / 主题。" }
  },
  "required": ["channel", "text"]
}`)

const sendNotificationWhenToUse = "用户要把某个结论 / 通知主动推送到飞书、钉钉等群里时用（比如\"把这段诊断发到运维群\"）。" +
	"channel 传“设置→通知”中配置的通知渠道名；它不是双向 IM 机器人。"

// Info — Class=write: it sends a real message (side-effecting, viewers can't
// use it) but it is not destructive.
func (t *SendNotificationTool) Info(_ context.Context) (*basetool.ToolInfo, error) {
	return &basetool.ToolInfo{
		Name:        ToolNameSendNotification,
		Description: "Send a message through a configured notification channel (Feishu / DingTalk / Slack / Telegram / WeCom). Pass the channel name from Settings → Notifications; this does not target a two-way IM bot.",
		WhenToUse:   sendNotificationWhenToUse,
		Parameters:  sendNotificationSchema,
		Class:       "write",
	}, nil
}

type sendNotificationArgs struct {
	Channel string `json:"channel"`
	Text    string `json:"text"`
	Title   string `json:"title"`
}

// InvokableRun resolves the channel by name (case-insensitive) and sends.
// A miss returns the available channel names so the LLM can self-correct.
func (t *SendNotificationTool) InvokableRun(ctx context.Context, argsJSON string, _ ...basetool.InvokeOption) (string, error) {
	if t.sender == nil {
		return "", fmt.Errorf("send_notification: channels not wired")
	}
	var in sendNotificationArgs
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return "", fmt.Errorf("send_notification: bad args: %w", err)
	}
	in.Channel = strings.TrimSpace(in.Channel)
	if in.Channel == "" || strings.TrimSpace(in.Text) == "" {
		return "", fmt.Errorf("send_notification: channel and text are required")
	}
	chans, err := t.sender.ListNotificationChannels(ctx)
	if err != nil {
		return "", fmt.Errorf("send_notification: list channels: %w", err)
	}
	var target *NotificationChannel
	for i := range chans {
		if strings.EqualFold(chans[i].Name, in.Channel) {
			target = &chans[i]
			break
		}
	}
	if target == nil {
		names := make([]string, 0, len(chans))
		for _, c := range chans {
			names = append(names, c.Name)
		}
		if len(names) == 0 {
			return "", fmt.Errorf("send_notification: no notification channels configured. Add one under Settings → Notifications first")
		}
		return "", fmt.Errorf("send_notification: channel %q not found. Available channels: %s", in.Channel, strings.Join(names, ", "))
	}
	if err := t.sender.SendNotification(ctx, target.ID, in.Title, in.Text); err != nil {
		return "", fmt.Errorf("send_notification: send to %q: %w", target.Name, err)
	}
	t.log.Info("send_notification: sent", slog.String("channel", target.Name), slog.String("kind", target.Kind))
	out, _ := json.Marshal(map[string]any{"sent": true, "channel": target.Name, "kind": target.Kind})
	return string(out), nil
}
