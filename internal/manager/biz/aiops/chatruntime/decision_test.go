package chatruntime

import "testing"

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
}
