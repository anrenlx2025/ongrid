package alert

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	model "github.com/ongridio/ongrid/internal/manager/model/alert"
	"github.com/ongridio/ongrid/internal/pkg/errs"
	"github.com/ongridio/ongrid/internal/pkg/logquery"
)

type recordingRuleCacheRefresher struct {
	calls int
	err   error
}

func (r *recordingRuleCacheRefresher) Refresh(_ context.Context) error {
	r.calls++
	return r.err
}

func TestMigrateLegacyLogRulesCanonicalizesPortableRules(t *testing.T) {
	repo := newFakeRepo()
	repo.rules["panic"] = &model.Rule{
		ID: 1, RuleKey: "panic", Kind: model.RuleKindLogMatch, ScopeType: model.RuleScopeGlobal,
		ConditionsJSON: `{"stream_selector":"{ongrid_source=~\"journald:.+\"}","line_filter":"(?i)panic|oom|fatal","window":"5m","operator":">=","threshold":1}`,
	}
	repo.rules["errors"] = &model.Rule{
		ID: 2, RuleKey: "errors", Kind: model.RuleKindLogVolume, ScopeType: model.RuleScopeGlobal,
		ConditionsJSON: `{"stream_selector":"{level=\"error\"}","window":"5m","ratio_op":">=","ratio_threshold":2}`,
	}
	cache := &recordingRuleCacheRefresher{}
	uc := NewUsecase(repo, nil)
	uc.SetRuleCacheRefresher(cache)

	count, err := uc.MigrateLegacyLogRules(t.Context())
	if err != nil {
		t.Fatalf("MigrateLegacyLogRules() error = %v", err)
	}
	if count != 2 || cache.calls != 1 {
		t.Fatalf("count=%d refresh calls=%d", count, cache.calls)
	}
	for _, key := range []string{"panic", "errors"} {
		rule := repo.rules[key]
		if rule.Kind != model.RuleKindLogSearch {
			t.Fatalf("%s kind = %q", key, rule.Kind)
		}
		if _, err := compileLogSearchRule(rule); err != nil {
			t.Fatalf("compile migrated %s: %v", key, err)
		}
	}
	var panicSpec logSearchSpec
	if err := json.Unmarshal([]byte(repo.rules["panic"].ConditionsJSON), &panicSpec); err != nil {
		t.Fatalf("decode migrated panic rule: %v", err)
	}
	if len(panicSpec.Filters) != 1 || panicSpec.Filters[0].Operator != logquery.FilterPrefix || panicSpec.Filters[0].Values[0] != "journald:" {
		t.Fatalf("migrated filters = %#v", panicSpec.Filters)
	}
}

func TestMigrateLegacyLogRulesRejectsHostScopedRuleWithoutWriting(t *testing.T) {
	repo := newFakeRepo()
	repo.rules["host_error"] = &model.Rule{
		ID: 1, RuleKey: "host_error", Kind: model.RuleKindLogMatch, ScopeType: model.RuleScopeHost,
		ConditionsJSON: `{"stream_selector":"{device_id=~\".+\"}","line_filter":"(?i)error","window":"5m","operator":">=","threshold":1}`,
	}
	_, err := NewUsecase(repo, nil).MigrateLegacyLogRules(t.Context())
	if !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("MigrateLegacyLogRules() error = %v", err)
	}
	if repo.rules["host_error"].Kind != model.RuleKindLogMatch {
		t.Fatalf("rule changed after failed migration: %#v", repo.rules["host_error"])
	}
}

func TestCreateRuleCanonicalizesLegacyInput(t *testing.T) {
	repo := newFakeRepo()
	row, err := NewUsecase(repo, nil).CreateRule(t.Context(), RuleInput{
		RuleKey: "legacy_client", Kind: model.RuleKindLogMatch, ScopeType: model.RuleScopeGlobal,
		Name: "Legacy client", Severity: "warning", Enabled: true,
		Spec: map[string]any{
			"stream_selector": `{level="error"}`, "line_filter": "(?i)timeout|panic",
			"window": "5m", "operator": ">=", "threshold": float64(1),
		},
	}, nil)
	if err != nil {
		t.Fatalf("CreateRule() error = %v", err)
	}
	if row.Kind != model.RuleKindLogSearch {
		t.Fatalf("stored kind = %q", row.Kind)
	}
	if _, err := compileLogSearchRule(row); err != nil {
		t.Fatalf("compile canonical rule: %v", err)
	}
}

func TestMigrateLegacyLogRulesSurfacesCacheRefreshFailure(t *testing.T) {
	repo := newFakeRepo()
	cache := &recordingRuleCacheRefresher{err: errors.New("refresh failed")}
	uc := NewUsecase(repo, nil)
	uc.SetRuleCacheRefresher(cache)
	count, err := uc.MigrateLegacyLogRules(t.Context())
	if count != 0 || err == nil || cache.calls != 1 {
		t.Fatalf("count=%d err=%v refresh calls=%d", count, err, cache.calls)
	}
}
