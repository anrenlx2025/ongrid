package store

import (
	"context"
	"errors"
	"testing"
	"time"

	model "github.com/ongridio/ongrid/internal/manager/model/edge"
	"github.com/ongridio/ongrid/internal/pkg/errs"
)

func TestEnrollmentClaimIsBoundedAndRetryable(t *testing.T) {
	edgeRepo := newTestRepo(t)
	repo := NewEnrollmentRepo(edgeRepo.db)
	ctx := context.Background()
	now := time.Now().UTC()
	profile := &model.EnrollmentProfile{
		Name:           "rack-a",
		AssignmentMode: model.EnrollmentModeBatchOnly,
		TokenHash:      "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ExpiresAt:      now.Add(time.Hour),
		MaxUses:        1,
		Status:         model.EnrollmentStatusActive,
	}
	if err := repo.CreateProfile(ctx, profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	first := &model.Edge{Name: "host-a", AccessKeyID: "access-a", SecretKeyHash: "hash-a", Status: model.StatusOffline}
	claimedProfile, enrollment, edge, created, err := repo.Claim(ctx, profile.TokenHash, "fp_host_a", "192.0.2.1", first, now)
	if err != nil {
		t.Fatalf("Claim(first): %v", err)
	}
	if !created || edge.ID == 0 || enrollment.EdgeID != edge.ID || claimedProfile.UsedCount != 1 {
		t.Fatalf("first claim = profile=%+v enrollment=%+v edge=%+v created=%v", claimedProfile, enrollment, edge, created)
	}

	retry := &model.Edge{Name: "host-a", AccessKeyID: "unused-new-access", SecretKeyHash: "hash-retry", Status: model.StatusOffline}
	claimedProfile, retryEnrollment, retryEdge, created, err := repo.Claim(ctx, profile.TokenHash, "fp_host_a", "192.0.2.1", retry, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Claim(retry): %v", err)
	}
	if created || retryEdge.ID != edge.ID || retryEdge.AccessKeyID != "access-a" || retryEdge.SecretKeyHash != "hash-retry" {
		t.Fatalf("retry claim = profile=%+v enrollment=%+v edge=%+v created=%v", claimedProfile, retryEnrollment, retryEdge, created)
	}
	if claimedProfile.UsedCount != 1 {
		t.Fatalf("retry used_count = %d, want 1", claimedProfile.UsedCount)
	}

	if err := repo.Complete(ctx, enrollment.ID, 42, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, _, _, _, err := repo.Claim(ctx, profile.TokenHash, "fp_host_a", "192.0.2.1", retry, now.Add(3*time.Minute)); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("completed retry error = %v, want ErrConflict", err)
	}
	second := &model.Edge{Name: "host-b", AccessKeyID: "access-b", SecretKeyHash: "hash-b", Status: model.StatusOffline}
	if _, _, _, _, err := repo.Claim(ctx, profile.TokenHash, "fp_host_b", "192.0.2.2", second, now.Add(3*time.Minute)); !errors.Is(err, errs.ErrBudgetExceeded) {
		t.Fatalf("quota error = %v, want ErrBudgetExceeded", err)
	}
}

func TestEnrollmentProfileRevokeStopsClaims(t *testing.T) {
	edgeRepo := newTestRepo(t)
	repo := NewEnrollmentRepo(edgeRepo.db)
	ctx := context.Background()
	profile := &model.EnrollmentProfile{
		Name:           "rack-b",
		AssignmentMode: model.EnrollmentModeBatchOnly,
		TokenHash:      "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		ExpiresAt:      time.Now().UTC().Add(time.Hour),
		MaxUses:        2,
		Status:         model.EnrollmentStatusActive,
	}
	if err := repo.CreateProfile(ctx, profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	if err := repo.RevokeProfile(ctx, profile.ID); err != nil {
		t.Fatalf("RevokeProfile: %v", err)
	}
	candidate := &model.Edge{Name: "host", AccessKeyID: "access", SecretKeyHash: "hash", Status: model.StatusOffline}
	if _, _, _, _, err := repo.Claim(ctx, profile.TokenHash, "fp_host", "", candidate, time.Now().UTC()); !errors.Is(err, errs.ErrUnauthorized) {
		t.Fatalf("revoked claim error = %v, want ErrUnauthorized", err)
	}
}

func TestEnrollmentProfileExpiryStopsClaims(t *testing.T) {
	edgeRepo := newTestRepo(t)
	repo := NewEnrollmentRepo(edgeRepo.db)
	ctx := context.Background()
	now := time.Now().UTC()
	profile := &model.EnrollmentProfile{
		Name:           "expired",
		AssignmentMode: model.EnrollmentModeBatchOnly,
		TokenHash:      "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		ExpiresAt:      now.Add(-time.Minute),
		MaxUses:        2,
		Status:         model.EnrollmentStatusActive,
	}
	if err := repo.CreateProfile(ctx, profile); err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	candidate := &model.Edge{Name: "host", AccessKeyID: "access", SecretKeyHash: "hash", Status: model.StatusOffline}
	if _, _, _, _, err := repo.Claim(ctx, profile.TokenHash, "fp_host", "", candidate, now); !errors.Is(err, errs.ErrUnauthorized) {
		t.Fatalf("expired claim error = %v, want ErrUnauthorized", err)
	}
}
