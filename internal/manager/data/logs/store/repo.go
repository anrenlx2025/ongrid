package store

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	model "github.com/ongridio/ongrid/internal/manager/model/logs"
	"github.com/ongridio/ongrid/internal/pkg/errs"
)

type Repo struct{ db *gorm.DB }

func NewRepo(db *gorm.DB) *Repo { return &Repo{db: db} }

func (r *Repo) SaveBackend(ctx context.Context, backend *model.Backend) error {
	if backend == nil {
		return errs.ErrInvalid
	}
	return r.db.WithContext(ctx).Save(backend).Error
}

func (r *Repo) GetBackend(ctx context.Context, id uint64) (*model.Backend, error) {
	var out model.Backend
	if err := r.db.WithContext(ctx).First(&out, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.ErrNotFound
		}
		return nil, err
	}
	return &out, nil
}

func (r *Repo) LatestBackend(ctx context.Context) (*model.Backend, error) {
	var out model.Backend
	if err := r.db.WithContext(ctx).Order("id DESC").First(&out).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.ErrNotFound
		}
		return nil, err
	}
	return &out, nil
}

func (r *Repo) ActiveBackend(ctx context.Context) (*model.Backend, error) {
	var out model.Backend
	if err := r.db.WithContext(ctx).
		Where("status IN ?", []model.BackendStatus{model.BackendStatusActive, model.BackendStatusRollingBack}).
		Order("id DESC").First(&out).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.ErrNotFound
		}
		return nil, err
	}
	return &out, nil
}

// ListQueryBackends returns every generation that owned the fleet data path
// for a non-empty interval. Retaining rolled-back rows lets the query planner
// route historical ranges to the backend that actually received those logs.
func (r *Repo) ListQueryBackends(ctx context.Context) ([]*model.Backend, error) {
	var out []*model.Backend
	err := r.db.WithContext(ctx).
		Where("cutover_at IS NOT NULL").
		Where("status IN ?", []model.BackendStatus{
			model.BackendStatusActive, model.BackendStatusRollingBack, model.BackendStatusRolledBack,
		}).
		Order("cutover_at ASC, id ASC").
		Find(&out).Error
	return out, err
}

// BeginRollout atomically moves one tested draft into distribution and
// replaces its selected Edge set. Previous attempts remain soft-deleted for
// audit while the active backend (if any) is left untouched.
func (r *Repo) BeginRollout(ctx context.Context, backend *model.Backend, assignments []*model.BackendAssignment) error {
	if backend == nil || backend.ID == 0 || len(assignments) == 0 {
		return errs.ErrInvalid
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&model.Backend{}).Where("id = ?", backend.ID).Updates(map[string]any{
			"status":                model.BackendStatusDistributing,
			"detected_version":      backend.DetectedVersion,
			"rollout_auto_activate": backend.RolloutAutoActivate,
			"last_test_at":          backend.LastTestAt,
			"last_error":            "",
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return errs.ErrNotFound
		}
		if err := tx.Where("backend_id = ?", backend.ID).Delete(&model.BackendAssignment{}).Error; err != nil {
			return err
		}
		for _, assignment := range assignments {
			if assignment == nil || assignment.BackendID != backend.ID || assignment.EdgeID == 0 {
				return errs.ErrInvalid
			}
			if err := tx.Create(assignment).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// BeginRollback keeps the Elasticsearch generation authoritative while the
// selected Edge set shadow-writes built-in Loki. ended_at remains unset until
// every real Loki write probe is visible through the Manager query path.
func (r *Repo) BeginRollback(ctx context.Context, backend *model.Backend, assignments []*model.BackendAssignment) error {
	if backend == nil || backend.ID == 0 || backend.CutoverAt == nil || len(assignments) == 0 {
		return errs.ErrInvalid
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&model.Backend{}).
			Where("id = ? AND status IN ?", backend.ID, []model.BackendStatus{
				model.BackendStatusActive, model.BackendStatusRollingBack,
			}).Updates(map[string]any{
			"status":                model.BackendStatusRollingBack,
			"rollout_auto_activate": true,
			"last_error":            "",
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return errs.ErrConflict
		}
		if err := tx.Where("backend_id = ?", backend.ID).Delete(&model.BackendAssignment{}).Error; err != nil {
			return err
		}
		for _, assignment := range assignments {
			if assignment == nil || assignment.BackendID != backend.ID || assignment.EdgeID == 0 {
				return errs.ErrInvalid
			}
			if err := tx.Create(assignment).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *Repo) ActivateBackend(ctx context.Context, id uint64, version string, cutover time.Time) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&model.Backend{}).
			Where("status = ? AND id <> ?", model.BackendStatusActive, id).
			Updates(map[string]any{
				"status":   model.BackendStatusRolledBack,
				"ended_at": cutover.UTC(),
			}).Error; err != nil {
			return err
		}
		result := tx.Model(&model.Backend{}).
			Where("id = ? AND status IN ?", id, []model.BackendStatus{
				model.BackendStatusDistributing, model.BackendStatusVerifying,
			}).
			Updates(map[string]any{
				"status":           model.BackendStatusActive,
				"detected_version": version,
				"cutover_at":       cutover.UTC(),
				"ended_at":         nil,
				"last_test_at":     cutover.UTC(),
				"last_error":       "",
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return errs.ErrConflict
		}
		return nil
	})
}

func (r *Repo) CompleteRollback(ctx context.Context, id uint64, endedAt time.Time) error {
	result := r.db.WithContext(ctx).Model(&model.Backend{}).
		Where("id = ? AND status = ?", id, model.BackendStatusRollingBack).
		Updates(map[string]any{
			"status":     model.BackendStatusRolledBack,
			"ended_at":   endedAt.UTC(),
			"last_error": "",
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errs.ErrConflict
	}
	return nil
}

// CancelBackend abandons a generation that never owned the authoritative
// data path. It deliberately leaves ended_at unset because there is no query
// interval to close.
func (r *Repo) CancelBackend(ctx context.Context, id uint64) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&model.Backend{}).
			Where("id = ? AND cutover_at IS NULL AND status IN ?", id, []model.BackendStatus{
				model.BackendStatusDraft, model.BackendStatusDistributing,
				model.BackendStatusVerifying, model.BackendStatusDegraded,
			}).Updates(map[string]any{
			"status":                model.BackendStatusRolledBack,
			"rollout_auto_activate": false,
			"ended_at":              nil,
			"last_error":            "",
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return errs.ErrConflict
		}
		return tx.Where("backend_id = ?", id).Delete(&model.BackendAssignment{}).Error
	})
}

func (r *Repo) SetBackendState(ctx context.Context, id uint64, status model.BackendStatus, version, lastError string, testedAt time.Time) error {
	fields := map[string]any{
		"status":           status,
		"detected_version": version,
		"last_error":       lastError,
	}
	if !testedAt.IsZero() {
		fields["last_test_at"] = testedAt.UTC()
	}
	result := r.db.WithContext(ctx).Model(&model.Backend{}).Where("id = ?", id).Updates(fields)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errs.ErrNotFound
	}
	return nil
}

// SetRolloutBackendState is intentionally conditional. A late/concurrent Edge
// acknowledgement must never move an already-active generation back to
// verifying or degraded after another acknowledgement completed promotion.
func (r *Repo) SetRolloutBackendState(ctx context.Context, id uint64, status model.BackendStatus, version, lastError string, testedAt time.Time) error {
	fields := map[string]any{
		"status":           status,
		"detected_version": version,
		"last_error":       lastError,
	}
	if !testedAt.IsZero() {
		fields["last_test_at"] = testedAt.UTC()
	}
	result := r.db.WithContext(ctx).Model(&model.Backend{}).
		Where("id = ? AND status IN ?", id, []model.BackendStatus{
			model.BackendStatusDistributing, model.BackendStatusVerifying, model.BackendStatusRollingBack,
		}).Updates(fields)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errs.ErrConflict
	}
	return nil
}

func (r *Repo) GetAssignment(ctx context.Context, backendID, edgeID uint64) (*model.BackendAssignment, error) {
	var out model.BackendAssignment
	if err := r.db.WithContext(ctx).
		Where("backend_id = ? AND edge_id = ?", backendID, edgeID).
		First(&out).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.ErrNotFound
		}
		return nil, err
	}
	return &out, nil
}

func (r *Repo) UpsertAssignment(ctx context.Context, assignment *model.BackendAssignment) error {
	if assignment == nil || assignment.BackendID == 0 || assignment.EdgeID == 0 {
		return errs.ErrInvalid
	}
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "backend_id"}, {Name: "edge_id"}, {Name: "delete_marker"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"desired_generation", "applied_generation", "status", "cutover_at",
			"last_probe_at", "last_write_success_at", "last_error", "updated_at",
		}),
	}).Create(assignment).Error
}

func (r *Repo) ListAssignments(ctx context.Context, backendID uint64) ([]*model.BackendAssignment, error) {
	var out []*model.BackendAssignment
	if err := r.db.WithContext(ctx).Where("backend_id = ?", backendID).Order("edge_id ASC").Find(&out).Error; err != nil {
		return nil, err
	}
	return out, nil
}
