package graph

import (
	"context"
	"encoding/json"
	"strconv"
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
	msg, err := m.inner.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	return pruneToolCallsForBudget(input, msg), nil
}

func (m *budgetStopModel) Stream(ctx context.Context, input []*schema.Message, opts ...einomodel.Option) (*schema.StreamReader[*schema.Message], error) {
	if msg, ok := finalAnswerAfterToolBudget(input); ok {
		return schema.StreamReaderFromArray([]*schema.Message{msg}), nil
	}
	return m.inner.Stream(ctx, input, opts...)
}

func pruneToolCallsForBudget(history []*schema.Message, msg *schema.Message) *schema.Message {
	if msg == nil || len(msg.ToolCalls) == 0 {
		return msg
	}
	perToolCounts, total := priorToolCounts(history)
	kept := make([]schema.ToolCall, 0, len(msg.ToolCalls))
	for _, call := range msg.ToolCalls {
		name := strings.TrimSpace(call.Function.Name)
		if name == "" {
			continue
		}
		if total >= maxTotalToolCallsPerRun {
			continue
		}
		if perToolCounts[name] >= maxCallsForTool(name) {
			continue
		}
		perToolCounts[name]++
		total++
		kept = append(kept, call)
	}
	if len(kept) == len(msg.ToolCalls) {
		return msg
	}
	cp := *msg
	cp.ToolCalls = kept
	if len(kept) == 0 {
		tool := "tool"
		if len(msg.ToolCalls) > 0 {
			tool = strings.TrimSpace(msg.ToolCalls[0].Function.Name)
		}
		cp.Content = budgetPrunedFinalContent(history, tool)
	}
	return &cp
}

func priorToolCounts(messages []*schema.Message) (map[string]int, int) {
	counts := make(map[string]int)
	total := 0
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg == nil {
			continue
		}
		if msg.Role == schema.User && !isSystemReminderMessage(msg.Content) {
			break
		}
		if msg.Role != schema.Tool {
			continue
		}
		name := strings.TrimSpace(msg.ToolName)
		if name == "" {
			continue
		}
		counts[name]++
		total++
	}
	return counts, total
}

func budgetPrunedFinalContent(messages []*schema.Message, tool string) string {
	if strings.TrimSpace(tool) == "" {
		tool = "tool"
	}
	evidence := summarizeRecentToolEvidence(messages)
	if wantsEnglishResponse(messages) {
		if evidence == "" {
			return "I stopped additional `" + tool + "` calls before execution because this turn reached its tool budget. The evidence collected so far is not enough for a confident conclusion; send a narrower target or time window and I can continue in the next message."
		}
		return "I stopped additional `" + tool + "` calls before execution because this turn reached its tool budget.\n\nBased on the evidence already collected:\n" + evidence + "\n\nNext step: use these findings as the current conclusion. If you need deeper proof, continue with a narrower target or time window."
	}
	if evidence == "" {
		return "我已在执行前停止继续调用 `" + tool + "`，因为本轮已经达到工具预算。当前证据还不足以形成可靠结论；请在下一条消息补充更明确的目标或时间窗，我再继续。"
	}
	return "我已在执行前停止继续调用 `" + tool + "`，因为本轮已经达到工具预算。\n\n当前结论先按本轮已经拿到的证据处理：\n" + evidence + "\n\n下一步：优先处理上述最明确的异常点；如果需要更深的证据，请在下一条消息指定更窄的目标或时间窗。"
}

func summarizeRecentToolEvidence(messages []*schema.Message) string {
	lines := make([]string, 0, 6)
	for i := len(messages) - 1; i >= 0 && len(lines) < 6; i-- {
		msg := messages[i]
		if msg == nil {
			continue
		}
		if msg.Role == schema.User && !isSystemReminderMessage(msg.Content) {
			break
		}
		if msg.Role != schema.Tool {
			continue
		}
		line := summarizeToolMessage(msg.ToolName, msg.Content)
		if line == "" {
			continue
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return ""
	}
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return "- " + strings.Join(lines, "\n- ")
}

func summarizeToolMessage(toolName, content string) string {
	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		toolName = "tool"
	}
	if toolName == "ToolSearch" || toolName == "get_edge_summary" {
		return ""
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(content), &payload); err == nil {
		switch toolName {
		case "query_incidents":
			return summarizeIncidentPayload(toolName, payload)
		case "correlate_incident":
			return summarizeCorrelationPayload(toolName, payload)
		case "host_du_summary":
			return summarizeDiskUsagePayload(toolName, payload)
		case "host_find_large_files":
			return summarizeLargeFilesPayload(toolName, payload)
		case "host_bash":
			return summarizeHostBashPayload(toolName, payload)
		}
		if count, ok := jsonNumber(payload["count"]); ok {
			return toolName + ": 返回 " + formatNumber(count) + " 条结果。"
		}
	}
	compact := strings.Join(strings.Fields(content), " ")
	if compact == "" {
		return ""
	}
	if len(compact) > 180 {
		compact = compact[:180] + "..."
	}
	return toolName + ": " + compact
}

func summarizeIncidentPayload(toolName string, payload map[string]any) string {
	count, _ := jsonNumber(payload["count"])
	incidents, _ := payload["incidents"].([]any)
	titles := make([]string, 0, 3)
	for _, item := range incidents {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		title, _ := obj["title"].(string)
		if title == "" {
			continue
		}
		titles = append(titles, trimRunes(title, 72))
		if len(titles) >= 3 {
			break
		}
	}
	if len(titles) == 0 {
		return toolName + ": 返回 " + formatNumber(count) + " 条告警。"
	}
	return toolName + ": 返回 " + formatNumber(count) + " 条告警，代表项：" + strings.Join(titles, "；")
}

func summarizeCorrelationPayload(toolName string, payload map[string]any) string {
	results, _ := payload["results"].([]any)
	parts := make([]string, 0, 3)
	for _, item := range results {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id, _ := jsonNumber(obj["incident_id"])
		bundle, _ := obj["bundle"].(map[string]any)
		incident, _ := bundle["incident"].(map[string]any)
		title, _ := incident["title"].(string)
		value, hasValue := jsonNumber(incident["value"])
		part := "incident " + formatNumber(id)
		if title != "" {
			part += " " + trimRunes(title, 56)
		}
		if hasValue {
			part += "，value=" + formatNumber(value)
		}
		parts = append(parts, part)
		if len(parts) >= 3 {
			break
		}
	}
	if len(parts) == 0 {
		return toolName + ": 返回关联分析结果。"
	}
	return toolName + ": " + strings.Join(parts, "；")
}

func summarizeDiskUsagePayload(toolName string, payload map[string]any) string {
	results, _ := payload["results"].([]any)
	parts := make([]string, 0, 4)
	for _, item := range results {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		path, _ := obj["path"].(string)
		subpaths, _ := obj["subpaths"].([]any)
		if path == "" || len(subpaths) == 0 {
			continue
		}
		top, _ := subpaths[0].(map[string]any)
		subpath, _ := top["subpath"].(string)
		size, _ := top["size_human"].(string)
		if subpath == "" {
			continue
		}
		if size != "" {
			parts = append(parts, path+" 最大项 "+subpath+"="+size)
		} else {
			parts = append(parts, path+" 最大项 "+subpath)
		}
		if len(parts) >= 4 {
			break
		}
	}
	if len(parts) == 0 {
		return toolName + ": 返回磁盘占用分析结果。"
	}
	return toolName + ": " + strings.Join(parts, "；")
}

func summarizeLargeFilesPayload(toolName string, payload map[string]any) string {
	results, _ := payload["results"].([]any)
	parts := make([]string, 0, 5)
	for _, item := range results {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		files, _ := obj["files"].([]any)
		for _, file := range files {
			fileObj, ok := file.(map[string]any)
			if !ok {
				continue
			}
			path, _ := fileObj["path"].(string)
			size, _ := fileObj["size_human"].(string)
			if path == "" {
				continue
			}
			if size != "" {
				parts = append(parts, path+"="+size)
			} else {
				parts = append(parts, path)
			}
			if len(parts) >= 5 {
				return toolName + ": 大文件 " + strings.Join(parts, "；")
			}
		}
	}
	if len(parts) == 0 {
		return toolName + ": 返回大文件扫描结果。"
	}
	return toolName + ": 大文件 " + strings.Join(parts, "；")
}

func summarizeHostBashPayload(toolName string, payload map[string]any) string {
	cmd, _ := payload["cmd"].(string)
	results, _ := payload["results"].([]any)
	stdouts := make([]string, 0, len(results))
	for _, item := range results {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		stdout, _ := obj["stdout"].(string)
		stdout = strings.TrimSpace(stdout)
		if stdout != "" {
			stdouts = append(stdouts, stdout)
		}
	}
	if len(stdouts) == 0 {
		return toolName + ": 命令 `" + trimRunes(cmd, 60) + "` 无有效输出。"
	}
	joined := strings.Join(stdouts, "\n")
	if strings.Contains(cmd, "df ") {
		line := firstDataLine(joined)
		if line != "" {
			return toolName + ": `df` 显示 " + trimRunes(line, 120)
		}
	}
	if strings.Contains(cmd, "du ") {
		tops := topOutputLines(joined, 4)
		if len(tops) > 0 {
			return toolName + ": `du` Top 项 " + strings.Join(tops, "；")
		}
	}
	lines := topOutputLines(joined, 3)
	if len(lines) == 0 {
		return toolName + ": 命令 `" + trimRunes(cmd, 60) + "` 已执行。"
	}
	return toolName + ": `" + trimRunes(cmd, 60) + "` 输出 " + strings.Join(lines, "；")
}

func firstDataLine(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" || strings.HasPrefix(strings.ToLower(line), "filesystem ") {
			continue
		}
		return line
	}
	return ""
}

func topOutputLines(output string, max int) []string {
	lines := make([]string, 0, max)
	for _, line := range strings.Split(output, "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			continue
		}
		lines = append(lines, trimRunes(line, 80))
		if len(lines) >= max {
			break
		}
	}
	return lines
}

func jsonNumber(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		n, err := v.Float64()
		return n, err == nil
	default:
		return 0, false
	}
}

func formatNumber(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func trimRunes(value string, max int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= max {
		return string(runes)
	}
	return string(runes[:max]) + "..."
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
