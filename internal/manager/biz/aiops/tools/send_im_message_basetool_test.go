package tools

import (
	"context"
	"strings"
	"testing"
)

func TestSendNotificationTool_WhenNoNotificationChannels_ExplainsConfigurationPath(t *testing.T) {
	tool := NewSendNotificationTool(fakeNotificationSender{}, nil)
	_, err := tool.InvokableRun(context.Background(), `{"channel":"ops","text":"alert"}`)
	if err == nil {
		t.Fatal("expected missing-channel error")
	}
	if !strings.Contains(err.Error(), "Settings → Notifications") {
		t.Fatalf("missing-channel error = %q, want Settings → Notifications guidance", err)
	}
}

func TestSendNotificationTool_InfoUsesNotificationWireName(t *testing.T) {
	info, err := NewSendNotificationTool(fakeNotificationSender{}, nil).Info(context.Background())
	if err != nil {
		t.Fatalf("tool info: %v", err)
	}
	if info.Name != ToolNameSendNotification {
		t.Fatalf("tool name = %q, want %q", info.Name, ToolNameSendNotification)
	}
}

type fakeNotificationSender struct{}

func (fakeNotificationSender) ListNotificationChannels(context.Context) ([]NotificationChannel, error) {
	return nil, nil
}
func (fakeNotificationSender) SendNotification(context.Context, uint64, string, string) error {
	return nil
}
