//go:build e2e

// Catalog: assistant chat — user-visible agent-loop contracts.
//
// These cases intentionally drive the public HTTP endpoints through a real
// manager process. Unit tests cover individual state transitions; this file
// protects the joins that previously regressed in production: session
// ownership, direct tool execution, progressive ToolSearch disclosure,
// streaming, and pre-execution clarification.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ongridio/ongrid/tests/e2e/testenv"
)

func TestAssistantChat_UserVisibleLoop(t *testing.T) {
	// Keep deferred schemas enabled even in the intentionally small E2E tool
	// registry. Production reaches this state once marketplace tools load; the
	// low threshold makes ToolSearch behaviour deterministic here.
	env := testenv.Start(t, testenv.WithEnv("ONGRID_TOOLBAG_DEFERRAL_THRESHOLD", "1"))
	admin := env.LoginAdmin()

	t.Run("direct answer persists root conversation and hides it from another user", func(t *testing.T) {
		env.FakeLLM().SetScript(testenv.LLMReply{Content: "E2E direct answer"})
		sessionID := createChatSession(t, env, admin.AccessToken, "E2E direct chat")

		status, body, err := env.DoJSON("POST", chatMessagesPath(sessionID), map[string]any{
			"content": "给我一个简短的健康摘要",
		}, admin.AccessToken)
		if err != nil || status != http.StatusOK {
			t.Fatalf("post message: status=%d body=%v err=%v", status, body, err)
		}
		if got := nestedString(body, "assistant_message", "content"); got != "E2E direct answer" {
			t.Fatalf("assistant content=%q body=%v", got, body)
		}

		status, body, err = env.DoJSON("GET", chatMessagesPath(sessionID), nil, admin.AccessToken)
		if err != nil || status != http.StatusOK {
			t.Fatalf("list messages: status=%d body=%v err=%v", status, body, err)
		}
		if got := int(body["total"].(float64)); got != 2 {
			t.Fatalf("message total=%d want 2; body=%v", got, body)
		}

		const (
			email    = "assistant-chat-other@ongrid.local"
			password = "E2E!assistant-chat-other"
		)
		mustCreateUser(t, env, admin.AccessToken, email, password, "user")
		other := env.Login(email, password)
		status, body, err = env.DoJSON("GET", chatMessagesPath(sessionID), nil, other.AccessToken)
		if err != nil || status != http.StatusNotFound {
			t.Fatalf("cross-user history: status=%d body=%v err=%v, want 404", status, body, err)
		}
	})

	t.Run("direct core tool closes the loop with real Prometheus evidence", func(t *testing.T) {
		env.FakeLLM().SetScript(
			testenv.LLMReply{ToolCalls: []testenv.LLMToolCall{{
				ID: "promql-1", Name: "query_promql", Arguments: `{"expr":"up"}`,
			}}},
			testenv.LLMReply{Content: "E2E PromQL evidence analyzed"},
		)
		sessionID := createChatSession(t, env, admin.AccessToken, "E2E PromQL")

		status, body, err := env.DoJSON("POST", chatMessagesPath(sessionID), map[string]any{
			"content": "查询 up 指标",
		}, admin.AccessToken)
		if err != nil || status != http.StatusOK {
			t.Fatalf("post PromQL: status=%d body=%v err=%v", status, body, err)
		}
		if got := nestedString(body, "assistant_message", "content"); got != "E2E PromQL evidence analyzed" {
			t.Fatalf("assistant content=%q body=%v", got, body)
		}
		if !containsToolCall(body["tool_calls"], "query_promql") {
			t.Fatalf("query_promql was not reported in tool_calls: %v", body)
		}
		requests := env.FakeLLM().Requests()
		if len(requests) != 2 || !containsString(requests[0].ToolNames, "query_promql") {
			t.Fatalf("LLM requests=%+v, want first turn to expose query_promql", requests)
		}
	})

	t.Run("ToolSearch reveals and executes a deferred tool", func(t *testing.T) {
		env.FakeLLM().SetScript(
			testenv.LLMReply{ToolCalls: []testenv.LLMToolCall{{
				ID: "search-1", Name: "ToolSearch", Arguments: `{"query":"select:query_alert_rules"}`,
			}}},
			testenv.LLMReply{ToolCalls: []testenv.LLMToolCall{{
				ID: "rules-1", Name: "query_alert_rules", Arguments: `{}`,
			}}},
			testenv.LLMReply{Content: "E2E alert rule inventory is empty"},
		)
		sessionID := createChatSession(t, env, admin.AccessToken, "E2E ToolSearch")

		status, body, err := env.DoJSON("POST", chatMessagesPath(sessionID), map[string]any{
			"content": "列出当前告警规则",
		}, admin.AccessToken)
		if err != nil || status != http.StatusOK {
			t.Fatalf("post ToolSearch: status=%d body=%v err=%v", status, body, err)
		}
		if got := nestedString(body, "assistant_message", "content"); got != "E2E alert rule inventory is empty" {
			t.Fatalf("assistant content=%q body=%v", got, body)
		}
		if !containsToolCall(body["tool_calls"], "ToolSearch") || !containsToolCall(body["tool_calls"], "query_alert_rules") {
			t.Fatalf("expected ToolSearch then query_alert_rules, body=%v", body)
		}
		requests := env.FakeLLM().Requests()
		if len(requests) != 3 || !containsString(requests[0].ToolNames, "ToolSearch") {
			t.Fatalf("LLM requests=%+v, want ToolSearch first", requests)
		}
	})

	t.Run("stream sends tool lifecycle and terminal summary", func(t *testing.T) {
		env.FakeLLM().SetScript(
			testenv.LLMReply{ToolCalls: []testenv.LLMToolCall{{
				ID: "stream-promql-1", Name: "query_promql", Arguments: `{"expr":"up"}`,
			}}},
			testenv.LLMReply{Content: "E2E streamed answer"},
		)
		sessionID := createChatSession(t, env, admin.AccessToken, "E2E streaming")
		events := postChatStream(t, env, admin.AccessToken, sessionID, "流式查询 up")
		for _, want := range []string{"assistant", "tool_start", "tool_end", "done", "summary"} {
			if !containsString(events, want) {
				t.Fatalf("SSE events=%v, missing %q", events, want)
			}
		}
	})

	t.Run("capture without an explicit target clarifies before model or tool execution", func(t *testing.T) {
		before := env.FakeLLM().CallCount()
		sessionID := createChatSession(t, env, admin.AccessToken, "E2E capture clarification")
		status, body, err := env.DoJSON("POST", chatMessagesPath(sessionID), map[string]any{
			"content": "抓 60 秒 tcp port 443 的包",
		}, admin.AccessToken)
		if err != nil || status != http.StatusOK {
			t.Fatalf("post capture clarification: status=%d body=%v err=%v", status, body, err)
		}
		if got := nestedString(body, "assistant_message", "content"); !strings.Contains(got, "设备") {
			t.Fatalf("clarification=%q, want explicit device question", got)
		}
		if after := env.FakeLLM().CallCount(); after != before {
			t.Fatalf("clarification called LLM: before=%d after=%d", before, after)
		}
		if calls, _ := body["tool_calls"].([]any); len(calls) != 0 {
			t.Fatalf("clarification executed tools: %v", calls)
		}
	})

	t.Run("stop interrupts an in-flight turn and leaves the session usable", func(t *testing.T) {
		block := env.FakeLLM().BlockNext()
		defer block.Release()
		sessionID := createChatSession(t, env, admin.AccessToken, "E2E stop")
		result := make(chan int, 1)
		go func() {
			status, _, err := env.DoJSON("POST", chatMessagesPath(sessionID), map[string]any{
				"content": "请进行一次需要较长时间的分析",
			}, admin.AccessToken)
			if err != nil {
				result <- 0
				return
			}
			result <- status
		}()

		waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := block.WaitStarted(waitCtx); err != nil {
			t.Fatalf("LLM turn did not start: %v", err)
		}
		status, body, err := env.DoJSON("POST", "/api/v1/chat/sessions/"+sessionID+"/stop", nil, admin.AccessToken)
		if err != nil || status != http.StatusOK || body["stopped"] != true {
			t.Fatalf("stop session: status=%d body=%v err=%v", status, body, err)
		}
		select {
		case <-result:
		case <-time.After(5 * time.Second):
			t.Fatal("cancelled turn did not return")
		}

		env.FakeLLM().SetScript(testenv.LLMReply{Content: "E2E turn after stop"})
		status, body, err = env.DoJSON("POST", chatMessagesPath(sessionID), map[string]any{
			"content": "取消后继续回答",
		}, admin.AccessToken)
		if err != nil || status != http.StatusOK || nestedString(body, "assistant_message", "content") != "E2E turn after stop" {
			t.Fatalf("post-stop retry: status=%d body=%v err=%v", status, body, err)
		}
	})

	t.Run("synchronous specialist work stays internal and returns to the root loop", func(t *testing.T) {
		status, sessions, err := env.DoJSON("GET", "/api/v1/chat/sessions", nil, admin.AccessToken)
		if err != nil || status != http.StatusOK {
			t.Fatalf("list sessions before delegation: status=%d body=%v err=%v", status, sessions, err)
		}
		before := int(sessions["total"].(float64))
		env.FakeLLM().SetScript(
			testenv.LLMReply{ToolCalls: []testenv.LLMToolCall{{
				ID: "delegate-1", Name: "AgentTool", Arguments: `{"description":"评估当前 SRE 风险","subagent_type":"specialist-sre","prompt":"检查当前告警和指标风险，返回简短证据。"}`,
			}}},
			testenv.LLMReply{Content: "E2E specialist evidence"},
			testenv.LLMReply{Content: "E2E root synthesized specialist result"},
		)
		sessionID := createChatSession(t, env, admin.AccessToken, "E2E delegated work")
		status, body, err := env.DoJSON("POST", chatMessagesPath(sessionID), map[string]any{
			"content": "请评估当前 SRE 风险并给我结论",
		}, admin.AccessToken)
		if err != nil || status != http.StatusOK {
			t.Fatalf("post delegated work: status=%d body=%v err=%v", status, body, err)
		}
		if got := nestedString(body, "assistant_message", "content"); got != "E2E root synthesized specialist result" {
			t.Fatalf("delegated root answer=%q body=%v", got, body)
		}
		if !containsToolCall(body["tool_calls"], "AgentTool") {
			t.Fatalf("AgentTool was not reported in tool_calls: %v", body)
		}
		status, sessions, err = env.DoJSON("GET", "/api/v1/chat/sessions", nil, admin.AccessToken)
		if err != nil || status != http.StatusOK {
			t.Fatalf("list sessions after delegation: status=%d body=%v err=%v", status, sessions, err)
		}
		if got := int(sessions["total"].(float64)); got != before+1 {
			t.Fatalf("visible sessions=%d want %d; internal worker leaked into chat list: %v", got, before+1, sessions)
		}
	})
}

func createChatSession(t *testing.T, env *testenv.Env, token, title string) string {
	t.Helper()
	status, body, err := env.DoJSON("POST", "/api/v1/chat/sessions", map[string]any{"title": title}, token)
	if err != nil || status != http.StatusCreated {
		t.Fatalf("create chat session: status=%d body=%v err=%v", status, body, err)
	}
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("create chat session returned empty id: %v", body)
	}
	t.Cleanup(func() {
		status, _, err := env.DoJSON("DELETE", "/api/v1/chat/sessions/"+id, nil, token)
		if err != nil || status != http.StatusNoContent {
			t.Logf("cleanup chat session %s: status=%d err=%v", id, status, err)
		}
	})
	return id
}

func chatMessagesPath(sessionID string) string {
	return "/api/v1/chat/sessions/" + sessionID + "/messages"
}

func postChatStream(t *testing.T, env *testenv.Env, token, sessionID, content string) []string {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"content": content})
	if err != nil {
		t.Fatalf("marshal stream payload: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, env.BaseURL()+chatMessagesPath(sessionID)+"/stream", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("create stream request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("stream status=%d body=%s", resp.StatusCode, raw)
	}

	var events []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			events = append(events, strings.TrimPrefix(line, "event: "))
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read SSE: %v", err)
	}
	return events
}

func nestedString(body map[string]any, key, nested string) string {
	value, _ := body[key].(map[string]any)
	out, _ := value[nested].(string)
	return out
}

func containsToolCall(raw any, name string) bool {
	calls, _ := raw.([]any)
	for _, rawCall := range calls {
		call, _ := rawCall.(map[string]any)
		if call["name"] == name {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
