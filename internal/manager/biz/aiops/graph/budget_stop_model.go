package graph

import (
	"context"
	"encoding/json"
	"strings"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

type budgetStopModel struct {
	inner einomodel.ToolCallingChatModel
}

func wrapBudgetStopModel(inner einomodel.ToolCallingChatModel) einomodel.ToolCallingChatModel {
	if inner == nil {
		return nil
	}
	return &budgetStopModel{inner: inner}
}

func (m *budgetStopModel) Generate(ctx context.Context, input []*schema.Message, opts ...einomodel.Option) (*schema.Message, error) {
	if msg, ok := finalAnswerAfterToolBudget(input); ok {
		return msg, nil
	}
	return m.inner.Generate(ctx, input, opts...)
}

func (m *budgetStopModel) Stream(ctx context.Context, input []*schema.Message, opts ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	if msg, ok := finalAnswerAfterToolBudget(input); ok {
		return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
	}
	return m.inner.Stream(ctx, input, opts...)
}

func (m *budgetStopModel) WithTools(tools []*schema.ToolInfo) (einomodel.ToolCallingChatModel, error) {
	next, err := m.inner.WithTools(tools)
	if err != nil {
		return nil, err
	}
	return &budgetStopModel{inner: next}, nil
}

func finalAnswerAfterToolBudget(messages []*schema.Message) (*schema.Message, bool) {
	env, ok := latestTerminalToolBudget(messages)
	if !ok {
		return nil, false
	}
	tool := strings.TrimSpace(env.Tool)
	if tool == "" {
		tool = "the tool"
	}
	content := "我已经停止继续调用 `" + tool + "`，避免在同一轮里反复查询。基于本轮已经拿到的结果：如果上面的数据已经出现异常信号，就按这些信号给出结论和下一步；如果结果为空或报错，本轮缺少可判定证据，请在下一条消息补充更具体的时间窗、service 或 device_id 后再查。"
	if wantsEnglishResponse(messages) {
		content = "I stopped calling `" + tool + "` again to avoid repeating the same investigation in this turn. Based on the evidence already collected: if the earlier results show an abnormal signal, use that signal for the conclusion and next step; if they were empty or errored, this turn lacks decisive evidence, so send a narrower time window, service, or device_id in the next message and I can query again."
	}
	return &schema.Message{Role: schema.Assistant, Content: content}, true
}

type toolBudgetEnvelope struct {
	Status      string `json:"status"`
	Tool        string `json:"tool"`
	FinalAnswer bool   `json:"final_answer_required"`
}

func latestTerminalToolBudget(messages []*schema.Message) (toolBudgetEnvelope, bool) {
	var zero toolBudgetEnvelope
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg == nil {
			continue
		}
		if msg.Role == schema.User && !isSystemReminderMessage(msg.Content) {
			return zero, false
		}
		if msg.Role != schema.Tool {
			continue
		}
		var env toolBudgetEnvelope
		if err := json.Unmarshal([]byte(msg.Content), &env); err != nil {
			continue
		}
		if env.Status == "call_budget_exceeded" && env.FinalAnswer {
			return env, true
		}
	}
	return zero, false
}

func isSystemReminderMessage(content string) bool {
	trimmed := strings.TrimSpace(strings.ToLower(content))
	return strings.HasPrefix(trimmed, "<system-reminder>")
}

func wantsEnglishResponse(messages []*schema.Message) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg == nil {
			continue
		}
		if msg.Role != schema.System && msg.Role != schema.User {
			continue
		}
		if strings.Contains(strings.ToLower(msg.Content), "respond in english") {
			return true
		}
	}
	return false
}
