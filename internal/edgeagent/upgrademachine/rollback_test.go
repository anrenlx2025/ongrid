package upgrademachine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRollback_RestoresPrevious(t *testing.T) {
	dest := t.TempDir()

	// 模拟 swap 后状态：新版本 + .previous 备份
	writeTestFile(t, filepath.Join(dest, "worker.exe"), "new-broken-worker")
	writeTestFile(t, filepath.Join(dest, "worker.exe.previous"), "old-good-worker")
	writeTestFile(t, filepath.Join(dest, "exporter.exe"), "new-exporter")
	writeTestFile(t, filepath.Join(dest, "exporter.exe.previous"), "old-exporter")

	restored, err := Rollback([]string{dest})
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if restored != 2 {
		t.Fatalf("expected 2 restored, got %d", restored)
	}

	// 验证恢复为旧内容
	got, _ := os.ReadFile(filepath.Join(dest, "worker.exe"))
	if string(got) != "old-good-worker" {
		t.Errorf("worker.exe = %q, want %q", got, "old-good-worker")
	}
	got, _ = os.ReadFile(filepath.Join(dest, "exporter.exe"))
	if string(got) != "old-exporter" {
		t.Errorf("exporter.exe = %q, want %q", got, "old-exporter")
	}

	// .previous 应被 rename 掉（不再存在）
	if _, err := os.Stat(filepath.Join(dest, "worker.exe.previous")); !os.IsNotExist(err) {
		t.Errorf("worker.exe.previous should be gone after rollback")
	}
}

func TestRollback_NoPreviousFiles(t *testing.T) {
	dest := t.TempDir()
	writeTestFile(t, filepath.Join(dest, "worker.exe"), "only-version")

	restored, err := Rollback([]string{dest})
	if err != nil {
		t.Fatalf("Rollback with no .previous should not error: %v", err)
	}
	if restored != 0 {
		t.Fatalf("expected 0 restored, got %d", restored)
	}
}

func TestRollback_MultipleDirs(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()

	writeTestFile(t, filepath.Join(dir1, "a.exe"), "new-a")
	writeTestFile(t, filepath.Join(dir1, "a.exe.previous"), "old-a")
	writeTestFile(t, filepath.Join(dir2, "b.exe"), "new-b")
	writeTestFile(t, filepath.Join(dir2, "b.exe.previous"), "old-b")

	restored, err := Rollback([]string{dir1, dir2})
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if restored != 2 {
		t.Fatalf("expected 2 restored across dirs, got %d", restored)
	}
}

func TestRollback_IgnoresNonPreviousFiles(t *testing.T) {
	dest := t.TempDir()
	writeTestFile(t, filepath.Join(dest, "worker.exe"), "current")
	writeTestFile(t, filepath.Join(dest, "readme.txt"), "info")
	writeTestFile(t, filepath.Join(dest, "worker.exe.previous"), "old")

	restored, _ := Rollback([]string{dest})
	if restored != 1 {
		t.Errorf("expected 1 restored (only .previous), got %d", restored)
	}

	// readme.txt 不应被动
	got, _ := os.ReadFile(filepath.Join(dest, "readme.txt"))
	if string(got) != "info" {
		t.Errorf("readme.txt should be untouched")
	}
}

func TestRollback_DirNotExist(t *testing.T) {
	restored, err := Rollback([]string{"/nonexistent/path/xyz"})
	if err != nil {
		t.Fatalf("Rollback on missing dir should not error: %v", err)
	}
	if restored != 0 {
		t.Errorf("expected 0 restored, got %d", restored)
	}
}

// TestRollback_SingleFileFailure_PropagatesError 验证：
// 单文件恢复失败必须使整体返回错误（保持可重试），而非 best-effort 吞错。
//
// 失败注入：worker.exe 预置为目录 → rename(worker.exe.previous, worker.exe)
// 跨平台必败（file → 已存在目录）。exporter.exe 正常，验证「失败不阻断其他
// 文件恢复」+「失败文件 .previous 保留供重试」。
func TestRollback_SingleFileFailure_PropagatesError(t *testing.T) {
	dest := t.TempDir()

	// worker.exe 是目录（rename 障碍）+ .previous 备份
	if err := os.Mkdir(filepath.Join(dest, "worker.exe"), 0o755); err != nil {
		t.Fatalf("mkdir obstacle: %v", err)
	}
	writeTestFile(t, filepath.Join(dest, "worker.exe.previous"), "old-worker")
	// exporter.exe 正常半换态
	writeTestFile(t, filepath.Join(dest, "exporter.exe"), "new-exporter")
	writeTestFile(t, filepath.Join(dest, "exporter.exe.previous"), "old-exporter")

	restored, err := Rollback([]string{dest})
	if err == nil {
		t.Fatal("expected error when single file rollback fails")
	}

	// 其他文件应已恢复（错误传播 ≠ 中断其他恢复）
	got, _ := os.ReadFile(filepath.Join(dest, "exporter.exe"))
	if string(got) != "old-exporter" {
		t.Errorf("exporter.exe = %q, want %q (other files must still restore)", got, "old-exporter")
	}
	// 失败文件的 .previous 必须保留（不写完成态 = 可重试）
	if _, statErr := os.Stat(filepath.Join(dest, "worker.exe.previous")); statErr != nil {
		t.Errorf("worker.exe.previous must be preserved for retry: %v", statErr)
	}
	if restored != 1 {
		t.Errorf("expected 1 restored (exporter only), got %d", restored)
	}

	// 障碍移除后重试 → 全部恢复成功
	if err := os.Remove(filepath.Join(dest, "worker.exe")); err != nil {
		t.Fatalf("remove obstacle dir: %v", err)
	}
	restored, err = Rollback([]string{dest})
	if err != nil {
		t.Fatalf("retry after obstacle removed should succeed: %v", err)
	}
	if restored != 1 {
		t.Fatalf("expected 1 restored on retry, got %d", restored)
	}
	got, _ = os.ReadFile(filepath.Join(dest, "worker.exe"))
	if string(got) != "old-worker" {
		t.Errorf("worker.exe = %q after retry, want %q", got, "old-worker")
	}
}

// TestRollbackAndMark_Failure_NoRollbackDone 验证：回滚失败时
// 不写 rollback.done 哨兵（半回滚状态保持可重试，而非被哨兵标记为完成）。
func TestRollbackAndMark_Failure_NoRollbackDone(t *testing.T) {
	stageDir := t.TempDir()
	binDir := t.TempDir()

	// worker.exe 是目录（rename 障碍）
	if err := os.Mkdir(filepath.Join(binDir, WorkerBinaryName), 0o755); err != nil {
		t.Fatalf("mkdir obstacle: %v", err)
	}
	writeTestFile(t, filepath.Join(binDir, WorkerBinaryName+PreviousSuffix), "old-worker")

	m := NewMachine(stageDir, binDir, testLogger(), nil)
	if err := m.RollbackAndMark(); err == nil {
		t.Fatal("expected RollbackAndMark to fail when rename blocked")
	}

	// 失败 → 不写 rollback.done（保持可重试）
	if _, err := os.Stat(filepath.Join(stageDir, RollbackDoneFile)); !os.IsNotExist(err) {
		t.Errorf("rollback.done must NOT be written on failed rollback, stat err = %v", err)
	}

	// 障碍移除后重试 → 成功 + 哨兵写入
	if err := os.Remove(filepath.Join(binDir, WorkerBinaryName)); err != nil {
		t.Fatalf("remove obstacle dir: %v", err)
	}
	if err := m.RollbackAndMark(); err != nil {
		t.Fatalf("retry RollbackAndMark: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stageDir, RollbackDoneFile)); err != nil {
		t.Errorf("rollback.done should exist after successful retry: %v", err)
	}
}

func TestCleanupPrevious_DeletesBackupFiles(t *testing.T) {
	dest := t.TempDir()
	writeTestFile(t, filepath.Join(dest, "worker.exe"), "good-new")
	writeTestFile(t, filepath.Join(dest, "worker.exe.previous"), "old-to-delete")
	writeTestFile(t, filepath.Join(dest, "exporter.exe.previous"), "old-to-delete")

	removed, err := CleanupPrevious([]string{dest})
	if err != nil {
		t.Fatalf("CleanupPrevious: %v", err)
	}
	if removed != 2 {
		t.Fatalf("expected 2 removed, got %d", removed)
	}

	// .previous 应被删除
	for _, f := range []string{"worker.exe.previous", "exporter.exe.previous"} {
		if _, err := os.Stat(filepath.Join(dest, f)); !os.IsNotExist(err) {
			t.Errorf("%s should be deleted", f)
		}
	}
	// 非 .previous 文件不动
	got, _ := os.ReadFile(filepath.Join(dest, "worker.exe"))
	if !strings.Contains(string(got), "good-new") {
		t.Errorf("worker.exe should be untouched")
	}
}
