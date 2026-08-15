package chatruntime

import (
	"reflect"
	"testing"
)

func TestDecide(t *testing.T) {
	tests := []struct {
		name  string
		facts ResolvedFacts
		want  Decision
	}{
		{name: "ambiguous target cannot execute", facts: ResolvedFacts{AmbiguousTarget: true, Permitted: true, LongRunning: true}, want: DecisionClarify},
		{name: "missing input clarifies", facts: ResolvedFacts{Missing: true, Permitted: true}, want: DecisionClarify},
		{name: "permission rejection wins", facts: ResolvedFacts{Permitted: false, Reason: "viewer"}, want: DecisionReject},
		{name: "confirmation precedes operation", facts: ResolvedFacts{Permitted: true, NeedsConfirmation: true, LongRunning: true}, want: DecisionPropose},
		{name: "long work creates operation", facts: ResolvedFacts{Permitted: true, LongRunning: true}, want: DecisionOperate},
		{name: "short work stays in loop", facts: ResolvedFacts{Permitted: true}, want: DecisionAct},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Decide(tt.facts); got != tt.want {
				t.Fatalf("Decide(%+v) = %q, want %q", tt.facts, got, tt.want)
			}
		})
	}
}

func TestConfirmedDeviceIDs(t *testing.T) {
	got := confirmedDeviceIDs([]Mention{{Type: "device", ID: "24"}, {Type: "alert", ID: "9"}, {Type: "device", ID: "bad"}, {Type: "DEVICE", ID: "25"}})
	if len(got) != 2 || got[0] != 24 || got[1] != 25 {
		t.Fatalf("confirmedDeviceIDs = %v", got)
	}
}

func TestResolveTurnRequiresCaptureTarget(t *testing.T) {
	plan, clarification := resolveTurn(&Request{UserText: "抓 60 秒 HTTPS 流量", Role: "admin"})
	if plan.Decision != DecisionClarify || plan.Phase != PhaseClarify || clarification == "" {
		t.Fatalf("resolveTurn missing target = %+v, %q", plan, clarification)
	}
	plan, _ = resolveTurn(&Request{UserText: "抓包", Role: "admin", Mentions: []Mention{{Type: "device", ID: "24"}}})
	if plan.Decision != DecisionOperate || plan.Phase != PhaseOperate || plan.Observe() != PhaseObserve {
		t.Fatalf("resolveTurn selected target = %+v", plan)
	}
	plan, clarification = resolveTurn(&Request{UserText: "抓 60 秒 tcp port 443 的包", Role: "admin"})
	if plan.Decision != DecisionClarify || clarification == "" {
		t.Fatalf("resolveTurn packet wording = %+v, %q", plan, clarification)
	}
	plan, clarification = resolveTurn(&Request{UserText: "停止刚才的抓包任务", Role: "admin"})
	if plan.Decision == DecisionClarify || clarification != "" {
		t.Fatalf("resolveTurn stop capture = %+v, %q; want agent loop", plan, clarification)
	}
}

func TestResolveTurnRequiresHostTargetForHostBoundOps(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{name: "disk", text: "磁盘快满了，帮我看看"},
		{name: "directory", text: "看看 /var 哪些目录最大"},
		{name: "process", text: "找一下占内存最高的进程"},
		{name: "interface", text: "检查网络接口错误包"},
		{name: "dns", text: "DNS 解析失败了，帮我排查"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, clarification := resolveTurn(&Request{UserText: tt.text, Role: "admin"})
			if plan.Decision != DecisionClarify || plan.Phase != PhaseClarify || clarification == "" {
				t.Fatalf("resolveTurn(%q) = %+v, %q; want clarify", tt.text, plan, clarification)
			}
		})
	}
}

func TestResolveTurnAllowsExplicitHostTarget(t *testing.T) {
	tests := []Request{
		{UserText: "看看 edge-001 的磁盘", Role: "admin"},
		{UserText: "检查 10.0.0.5 的网络接口", Role: "admin"},
		{UserText: "看看目录 /var", Role: "admin", Mentions: []Mention{{Type: "device", ID: "24"}}},
		{UserText: "device_id=24 查大文件", Role: "admin"},
	}
	for _, req := range tests {
		plan, clarification := resolveTurn(&req)
		if plan.Decision == DecisionClarify || clarification != "" {
			t.Fatalf("resolveTurn(%q) = %+v, %q; want non-clarify", req.UserText, plan, clarification)
		}
	}
}

func TestTurnPlanRecordsAndLoopsThroughSystemStates(t *testing.T) {
	plan := PlanTurn(ResolvedFacts{Permitted: true, LongRunning: true})
	want := []TurnPhase{PhaseUnderstand, PhaseResolve, PhaseDecide, PhaseOperate}
	if !reflect.DeepEqual(plan.Transitions, want) {
		t.Fatalf("Transitions = %v, want %v", plan.Transitions, want)
	}
	if plan.Observe() != PhaseObserve || plan.NextAfterObserve() != PhaseUnderstand {
		t.Fatalf("observe loop = %q -> %q, want observe -> understand", plan.Observe(), plan.NextAfterObserve())
	}

	clarify := PlanTurn(ResolvedFacts{Permitted: true, Missing: true})
	if clarify.NextAfterObserve() != PhaseClarify {
		t.Fatalf("non-executable plan looped to %q, want clarify", clarify.NextAfterObserve())
	}
}
