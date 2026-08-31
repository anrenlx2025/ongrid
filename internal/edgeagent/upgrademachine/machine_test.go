// machine_test.go 测试 Machine 深模块的编排逻辑。
//
// 从 cmd/upgrade_windows_test.go 迁移，适配 Machine API：
//   - applyAndSwap → Machine.Apply
//   - maybeApplyOnBoot / maybeRollbackOnBoot → Machine.BootCheck
//   - checkPendingUpgrade → Machine.CheckPending
//   - watchUpgradeHealth → Machine.HealthPoll (renamed from HealthCheck)
//   - rollbackAndMark → Machine.RollbackAndMark
//
// 纯 Go（无 Windows 专属依赖），在 Linux CI 可跑。

package upgrademachine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// --- 测试辅助 ---

// testLogger 返回一个日志级别设为 100（高于所有级别）的 Logger，
// 实际效果是抑制所有日志输出。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.Level(100)}))
}

// testLoggerCapturing 返回一个写入 buf 的 WARN 级别 Logger，
// 用于断言被测代码是否输出了期望的 warn 日志（显式失败原则）。
func testLoggerCapturing(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// mockProcessController 是 ProcessController 的测试 mock。
type mockProcessController struct {
	killTreeCalls   atomic.Int32
	killTreeLastPID atomic.Int32
	killImageCalls  atomic.Int32
	killImageNames  []string
	treeErr         error // 非 nil 时 KillTree 返回此 error
}

func (m *mockProcessController) KillTree(pid int) error {
	m.killTreeCalls.Add(1)
	m.killTreeLastPID.Store(int32(pid))
	if m.treeErr != nil {
		return m.treeErr
	}
	return nil
}

func (m *mockProcessController) KillByImage(name string) error {
	m.killImageCalls.Add(1)
	m.killImageNames = append(m.killImageNames, name)
	return nil
}

// buildMachineBundle 创建完整 fake bundle：stageDir/incoming/ 下有 MANIFEST.txt
// + src 文件 + VERSION。destDir 模拟部署目标（已有旧版本）。
func buildMachineBundle(t *testing.T, stageDir, destDir, version string,
	files []struct{ Src, Dest, Content string }) {

	t.Helper()
	incoming := filepath.Join(stageDir, IncomingDirName)

	var lines []string
	for _, f := range files {
		sha := writeTestFile(t, filepath.Join(incoming, f.Src), f.Content)
		lines = append(lines, sha+" 0755 "+f.Src+" "+filepath.Join(destDir, f.Dest))
	}
	// MANIFEST.txt
	manifestContent := strings.Join(lines, "\n") + "\n"
	writeTestFile(t, filepath.Join(incoming, ManifestFileName), manifestContent)
	// VERSION
	writeTestFile(t, filepath.Join(incoming, VersionFileName), version)
}

// --- Machine.Apply ---

func TestMachine_Apply_KillCalledWhenPIDPositive(t *testing.T) {
	stageDir := t.TempDir()
	destDir := t.TempDir()
	writeTestFile(t, filepath.Join(destDir, WorkerBinaryName), "old")
	buildMachineBundle(t, stageDir, destDir, "v1.0.0", []struct{ Src, Dest, Content string }{
		{WorkerBinaryName, WorkerBinaryName, "new"},
	})

	var pc mockProcessController
	m := NewMachine(stageDir, destDir, testLogger(), &pc)
	if err := m.Apply(context.Background(), 12345); err != nil {
		t.Fatalf("Machine.Apply: %v", err)
	}
	if pc.killTreeCalls.Load() != 1 {
		t.Errorf("KillTree called %d times, want 1", pc.killTreeCalls.Load())
	}
	if pc.killTreeLastPID.Load() != 12345 {
		t.Errorf("KillTree called with pid=%d, want 12345", pc.killTreeLastPID.Load())
	}
}

func TestMachine_Apply_KillSkippedWhenPIDZero(t *testing.T) {
	stageDir := t.TempDir()
	destDir := t.TempDir()
	writeTestFile(t, filepath.Join(destDir, WorkerBinaryName), "old")
	buildMachineBundle(t, stageDir, destDir, "v1.0.0", []struct{ Src, Dest, Content string }{
		{WorkerBinaryName, WorkerBinaryName, "new"},
	})

	var pc mockProcessController
	m := NewMachine(stageDir, destDir, testLogger(), &pc)
	// PID=0 → 应跳过 kill
	if err := m.Apply(context.Background(), 0); err != nil {
		t.Fatalf("Machine.Apply: %v", err)
	}
	if pc.killTreeCalls.Load() != 0 {
		t.Errorf("KillTree should NOT be called when PID=0")
	}
}

// TestKillManifestExes_SkipsSupervisorBinary 验证 KillManifestExes 不 kill
// supervisor 自己。MANIFEST 包含 supervisor.exe 用于 rename-aside 自升级，
// 但 kill supervisor 进程会导致 SCM restart 死循环。
func TestKillManifestExes_SkipsSupervisorBinary(t *testing.T) {
	entries := []ManifestEntry{
		{Dest: `C:\bin\` + WorkerBinaryName},
		{Dest: `C:\bin\windows_exporter.exe`},
		{Dest: `C:\bin\` + SupervisorBinaryName},
	}
	var pc mockProcessController
	m := NewMachine(t.TempDir(), t.TempDir(), testLogger(), &pc)

	m.KillManifestExes(entries)

	if len(pc.killImageNames) != 2 {
		t.Fatalf("KillByImage should be called 2 times (worker + exporter), got %d: %v",
			len(pc.killImageNames), pc.killImageNames)
	}
	for _, name := range pc.killImageNames {
		if name == SupervisorBinaryName {
			t.Errorf("KillByImage must NOT be called for %q (supervisor self-kill)", SupervisorBinaryName)
		}
	}
}

func TestMachine_Apply_KillErrorIgnored(t *testing.T) {
	stageDir := t.TempDir()
	destDir := t.TempDir()
	writeTestFile(t, filepath.Join(destDir, WorkerBinaryName), "old")
	buildMachineBundle(t, stageDir, destDir, "v1.0.0", []struct{ Src, Dest, Content string }{
		{WorkerBinaryName, WorkerBinaryName, "new"},
	})

	pc := &mockProcessController{treeErr: errors.New("ERROR: not found")}
	m := NewMachine(stageDir, destDir, testLogger(), pc)
	// KillTree 报错但 Apply 应继续（幂等语义）
	if err := m.Apply(context.Background(), 12345); err != nil {
		t.Fatalf("Machine.Apply should ignore kill error: %v", err)
	}
}

func TestMachine_Apply_FullOrdering(t *testing.T) {
	stageDir := t.TempDir()
	destDir := t.TempDir()
	writeTestFile(t, filepath.Join(destDir, WorkerBinaryName), "old-worker")
	writeTestFile(t, filepath.Join(destDir, "windows_exporter.exe"), "old-exporter")
	buildMachineBundle(t, stageDir, destDir, "v2.0.0", []struct{ Src, Dest, Content string }{
		{WorkerBinaryName, WorkerBinaryName, "new-worker"},
		{"windows_exporter.exe", "windows_exporter.exe", "new-exporter"},
	})

	m := NewMachine(stageDir, destDir, testLogger(), nil)
	if err := m.Apply(context.Background(), 0); err != nil {
		t.Fatalf("Machine.Apply: %v", err)
	}

	// 验证 swap：dest 内容是新版本
	got, _ := os.ReadFile(filepath.Join(destDir, WorkerBinaryName))
	if string(got) != "new-worker" {
		t.Errorf("worker.exe = %q, want %q", got, "new-worker")
	}
	got, _ = os.ReadFile(filepath.Join(destDir, "windows_exporter.exe"))
	if string(got) != "new-exporter" {
		t.Errorf("exporter.exe = %q, want %q", got, "new-exporter")
	}

	// 验证 .previous 备份存在（旧版本）
	got, _ = os.ReadFile(filepath.Join(destDir, WorkerBinaryName+PreviousSuffix))
	if string(got) != "old-worker" {
		t.Errorf("worker.exe.previous = %q, want %q", got, "old-worker")
	}

	// 验证 incoming/ 已删除
	if _, err := os.Stat(filepath.Join(stageDir, IncomingDirName)); !os.IsNotExist(err) {
		t.Error("incoming/ should be removed after Apply")
	}

	// 验证 last_upgrade_ver 已写
	ver, _ := os.ReadFile(filepath.Join(stageDir, LastUpgradeVerFile))
	if string(ver)[:len("v2.0.0")] != "v2.0.0" {
		t.Errorf("last_upgrade_ver = %q, want v2.0.0", ver)
	}

	// 验证 healthy_marker 已删（重新武装 watchdog）
	if _, err := os.Stat(filepath.Join(stageDir, HealthyMarkerFile)); !os.IsNotExist(err) {
		t.Error("healthy_marker should be removed after Apply")
	}
}

func TestMachine_Apply_BadManifest(t *testing.T) {
	stageDir := t.TempDir()
	writeTestFile(t, filepath.Join(stageDir, IncomingDirName, ManifestFileName), "bad line")

	m := NewMachine(stageDir, t.TempDir(), testLogger(), nil)
	err := m.Apply(context.Background(), 0)
	if err == nil {
		t.Fatal("expected error for malformed manifest")
	}
}

// --- Machine.BootCheck ---

func TestMachine_BootCheck_NoPending(t *testing.T) {
	stageDir := t.TempDir()
	binDir := t.TempDir()
	m := NewMachine(stageDir, binDir, testLogger(), nil)
	if err := m.BootCheck(context.Background()); err != nil {
		t.Fatalf("should be no-op when no pending: %v", err)
	}
}

func TestMachine_BootCheck_PendingApplied(t *testing.T) {
	stageDir := t.TempDir()
	destDir := t.TempDir()
	writeTestFile(t, filepath.Join(destDir, WorkerBinaryName), "old")
	buildMachineBundle(t, stageDir, destDir, "v1.0.0", []struct{ Src, Dest, Content string }{
		{WorkerBinaryName, WorkerBinaryName, "new"},
	})

	m := NewMachine(stageDir, destDir, testLogger(), nil)
	if err := m.BootCheck(context.Background()); err != nil {
		t.Fatalf("BootCheck: %v", err)
	}
	if IsPending(stageDir) {
		t.Error("pending should be cleared after boot apply")
	}
}

func TestMachine_BootCheck_NeverUpgraded(t *testing.T) {
	stageDir := t.TempDir()
	binDir := t.TempDir()
	m := NewMachine(stageDir, binDir, testLogger(), nil)
	if err := m.BootCheck(context.Background()); err != nil {
		t.Fatalf("should be no-op when never upgraded: %v", err)
	}
}

func TestMachine_BootCheck_Healthy(t *testing.T) {
	stageDir := t.TempDir()
	binDir := t.TempDir()
	writeTestFile(t, filepath.Join(stageDir, LastUpgradeVerFile), "v1.0.0")
	writeTestFile(t, filepath.Join(stageDir, HealthyMarkerFile), "v1.0.0")

	m := NewMachine(stageDir, binDir, testLogger(), nil)
	if err := m.BootCheck(context.Background()); err != nil {
		t.Fatalf("should be no-op when healthy: %v", err)
	}
}

func TestMachine_BootCheck_UnhealthyRollsBack(t *testing.T) {
	stageDir := t.TempDir()
	binDir := t.TempDir()
	writeTestFile(t, filepath.Join(stageDir, LastUpgradeVerFile), "v2.0.0")
	writeTestFile(t, filepath.Join(stageDir, HealthyMarkerFile), "v1.0.0")
	writeTestFile(t, filepath.Join(binDir, WorkerBinaryName), "new-broken")
	writeTestFile(t, filepath.Join(binDir, WorkerBinaryName+PreviousSuffix), "old-good")

	m := NewMachine(stageDir, binDir, testLogger(), nil)
	if err := m.BootCheck(context.Background()); err != nil {
		t.Fatalf("BootCheck: %v", err)
	}

	got, _ := os.ReadFile(filepath.Join(binDir, WorkerBinaryName))
	if string(got) != "old-good" {
		t.Errorf("worker.exe = %q, want %q (rolled back)", got, "old-good")
	}
	if _, err := os.Stat(filepath.Join(stageDir, RollbackDoneFile)); err != nil {
		t.Errorf("rollback.done sentinel should exist: %v", err)
	}
}

func TestMachine_BootCheck_RollbackDoneSkips(t *testing.T) {
	stageDir := t.TempDir()
	binDir := t.TempDir()
	writeTestFile(t, filepath.Join(stageDir, RollbackDoneFile), "done")
	writeTestFile(t, filepath.Join(stageDir, LastUpgradeVerFile), "v2.0.0")
	writeTestFile(t, filepath.Join(stageDir, HealthyMarkerFile), "v1.0.0")
	writeTestFile(t, filepath.Join(binDir, WorkerBinaryName), "new-broken")
	writeTestFile(t, filepath.Join(binDir, WorkerBinaryName+PreviousSuffix), "old-good")

	m := NewMachine(stageDir, binDir, testLogger(), nil)
	if err := m.BootCheck(context.Background()); err != nil {
		t.Fatalf("should be no-op when rollback.done present: %v", err)
	}

	got, _ := os.ReadFile(filepath.Join(binDir, WorkerBinaryName))
	if string(got) != "new-broken" {
		t.Errorf("worker.exe = %q, should remain unchanged", got)
	}
}

// --- Machine.CheckPending ---

// TestMachine_BootCheck_ExtractsPendingBundle 验证 BootCheck 在 incoming/ 为空但
// pending tar.gz 存在时自动解压再 apply。
func TestMachine_BootCheck_ExtractsPendingBundle(t *testing.T) {
	stageDir := t.TempDir()
	destDir := t.TempDir()

	writeTestFile(t, filepath.Join(destDir, WorkerBinaryName), "old")
	// 写 pending tar.gz（不含 incoming/，模拟 Windows 无 ExecStartPre 的场景）
	h := sha256.Sum256([]byte("new"))
	sha := hex.EncodeToString(h[:])
	manifestContent := sha + " 0755 " + WorkerBinaryName + " " + filepath.Join(destDir, WorkerBinaryName) + "\n"
	writePendingBundle(t, stageDir, map[string]string{
		"MANIFEST.txt":   manifestContent,
		"VERSION":        "v9.9.9",
		WorkerBinaryName: "new",
	})

	m := NewMachine(stageDir, destDir, testLogger(), &mockProcessController{})
	_ = m.BootCheck(context.Background())

	// pending tar.gz 应被删除（解压后清理）
	if _, err := os.Stat(filepath.Join(stageDir, PendingFileName)); !os.IsNotExist(err) {
		t.Errorf("pending should be deleted after BootCheck extraction, got err: %v", err)
	}
	// worker.exe 应被更新（apply 成功）
	data, err := os.ReadFile(filepath.Join(destDir, WorkerBinaryName))
	if err != nil {
		t.Fatalf("read worker.exe after BootCheck: %v", err)
	}
	if string(data) != "new" {
		t.Errorf("worker.exe not updated after BootCheck pending extraction, got %q", string(data))
	}
}

// TestMachine_CheckPending_ExtractsPendingBundle 验证 CheckPending 在 incoming/ 为空但
// pending tar.gz 存在时自动解压再 apply。
func TestMachine_CheckPending_ExtractsPendingBundle(t *testing.T) {
	stageDir := t.TempDir()
	destDir := t.TempDir()

	writeTestFile(t, filepath.Join(destDir, WorkerBinaryName), "old")
	h := sha256.Sum256([]byte("new"))
	sha := hex.EncodeToString(h[:])
	manifestContent := sha + " 0755 " + WorkerBinaryName + " " + filepath.Join(destDir, WorkerBinaryName) + "\n"
	writePendingBundle(t, stageDir, map[string]string{
		"MANIFEST.txt":   manifestContent,
		"VERSION":        "v9.9.9",
		WorkerBinaryName: "new",
	})

	m := NewMachine(stageDir, destDir, testLogger(), &mockProcessController{})
	err := m.CheckPending(context.Background(), 123)
	if !errors.Is(err, ErrApplied) {
		t.Errorf("expected ErrApplied after pending extraction, got %v", err)
	}
	// pending tar.gz 应被删除
	if _, err := os.Stat(filepath.Join(stageDir, PendingFileName)); !os.IsNotExist(err) {
		t.Errorf("pending should be deleted after CheckPending extraction, got err: %v", err)
	}
}

func TestMachine_CheckPending_NoPending(t *testing.T) {
	stageDir := t.TempDir()
	m := NewMachine(stageDir, t.TempDir(), testLogger(), &mockProcessController{})
	if err := m.CheckPending(context.Background(), 123); err != nil {
		t.Fatalf("should return nil when no pending: %v", err)
	}
}

func TestMachine_CheckPending_PendingReturnsSentinel(t *testing.T) {
	stageDir := t.TempDir()
	destDir := t.TempDir()

	writeTestFile(t, filepath.Join(destDir, WorkerBinaryName), "old")
	buildMachineBundle(t, stageDir, destDir, "v1.0.0", []struct{ Src, Dest, Content string }{
		{WorkerBinaryName, WorkerBinaryName, "new"},
	})

	m := NewMachine(stageDir, destDir, testLogger(), &mockProcessController{})
	err := m.CheckPending(context.Background(), 123)
	if !errors.Is(err, ErrApplied) {
		t.Errorf("expected ErrApplied, got %v", err)
	}
}

// --- Machine.HealthPoll ---
//
// 设计要点：HealthPoll 只做 polling + IsUpgradeHealthy + CleanupPrevious +
// 清 awaiting_health sentinel（成功路径），不持 timer、不直接调用
// RollbackAndMark — rollback 决策统一放在 superviseWorker 循环顶部
// （worker 已退出的空窗执行，rename 不会撞文件锁）。
//
// 注意：timer 触发的 rollback 决策在 superviseWorker 侧（worker_test.go 覆盖），
// HealthPoll 自身无超时分支。

func TestMachine_HealthPoll_HealthyCleanup(t *testing.T) {
	stageDir := t.TempDir()
	binDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	writeTestFile(t, filepath.Join(stageDir, LastUpgradeVerFile), "v1.0.0")
	writeTestFile(t, filepath.Join(stageDir, HealthyMarkerFile), "v1.0.0")
	writeTestFile(t, filepath.Join(binDir, WorkerBinaryName+PreviousSuffix), "old")

	m := NewMachine(stageDir, binDir, testLogger(), nil)
	// 短 poll（50ms）确保轮询快速发现健康状态
	err := m.HealthPoll(ctx, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("expected nil (healthy), got %v", err)
	}

	if _, err := os.Stat(filepath.Join(binDir, WorkerBinaryName+PreviousSuffix)); !os.IsNotExist(err) {
		t.Error(".previous should be cleaned up after healthy confirmation")
	}
}

func TestMachine_HealthPoll_ContextCancelled(t *testing.T) {
	stageDir := t.TempDir()
	binDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())

	writeTestFile(t, filepath.Join(stageDir, LastUpgradeVerFile), "v2.0.0")
	writeTestFile(t, filepath.Join(stageDir, HealthyMarkerFile), "v1.0.0")

	cancel()
	m := NewMachine(stageDir, binDir, testLogger(), nil)
	err := m.HealthPoll(ctx, 50*time.Millisecond)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

// --- awaiting-health：BootCheck supervisor_selfswap_awaiting_health sentinel ---
//
// self-swap 刚完成的启动：BootCheck 步骤 1 不立即 rollback
// （worker 尚未启动，没有健康标记不等于升级失败），而是检测 awaiting_health
// sentinel → arm HealthCheck（设 pendingHealthCheck=true），
// 让 superviseWorker 启 worker 后用 HealthCheck 180s grace 确认健康。

// TestMachine_BootCheck_SelfSwapAwaitingHealth_ArmsHealthCheck 验证 awaiting-health 分支：
// supervisor self-swap 完成后的文件系统状态（lastVer 写入 + marker 删除 + awaiting
// sentinel 写入）下，BootCheck 应 arm HealthCheck 而非立即 rollback。
//
// 状态模拟（SupervisorSelfSwap step 3 成功后的真机状态，见  实证）：
//   - last_upgrade_ver = "dev"（WriteUpgradeMeta 写入）
//   - healthy_marker 缺失（WriteUpgradeMeta 删除）
//   - supervisor_selfswap_awaiting_health sentinel 存在（SelfSwap step 3 写入）
//
// 期望：BootCheck 不 rollback + m.PendingHealthCheck==true。
func TestMachine_BootCheck_SelfSwapAwaitingHealth_ArmsHealthCheck(t *testing.T) {
	stageDir := t.TempDir()
	binDir := t.TempDir()

	// self-swap 后状态：lastVer 写 + marker 缺 + awaiting sentinel 写
	writeTestFile(t, filepath.Join(stageDir, LastUpgradeVerFile), "dev")
	// healthy_marker 故意不写（WriteUpgradeMeta 已删）
	writeTestFile(t, filepath.Join(stageDir, SupervisorSelfSwapAwaitingHealthFile), "awaiting")
	// worker.exe 新版本（self-swap 后的状态），不应被 rollback
	writeTestFile(t, filepath.Join(binDir, WorkerBinaryName), "new-supervisor-swap")

	m := NewMachine(stageDir, binDir, testLogger(), nil)
	if err := m.BootCheck(context.Background()); err != nil {
		t.Fatalf("BootCheck: %v", err)
	}

	// awaiting-health：应 arm HealthCheck
	if !m.PendingHealthCheck() {
		t.Error("PendingHealthCheck() = false, want true (awaiting-health 应 arm HealthCheck)")
	}

	// rollback.done 不应存在（未 rollback）
	if _, err := os.Stat(filepath.Join(stageDir, RollbackDoneFile)); err == nil {
		t.Error("rollback.done sentinel 不应存在（awaiting-health 跳过 rollback）")
	}

	// worker.exe 不应被回滚（仍是新版本）
	got, _ := os.ReadFile(filepath.Join(binDir, WorkerBinaryName))
	if string(got) != "new-supervisor-swap" {
		t.Errorf("worker.exe = %q, want %q (awaiting-health 不应 rollback)",
			got, "new-supervisor-swap")
	}

	// awaiting_health sentinel 应保留（HealthCheck 解决时才清，BootCheck 不清）
	if _, err := os.Stat(filepath.Join(stageDir, SupervisorSelfSwapAwaitingHealthFile)); err != nil {
		t.Error("supervisor_selfswap_awaiting_health sentinel 应保留（BootCheck 不清，HealthCheck 解决时清）")
	}
}

// TestMachine_HealthPoll_Success_DeletesSupervisorOld 验证：
// .old 的唯一权威删除点 = HealthPoll 成功路径，与 worker 备份清理
// （CleanupPrevious）同点同趟。健康确认前 .old 由 BootCheck 条件化保护
// （见 TestBootCheck_AppliedAwaitingHealth_PreservesOld）。
func TestMachine_HealthPoll_Success_DeletesSupervisorOld(t *testing.T) {
	stageDir := t.TempDir()
	binDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 健康状态：lastVer == marker
	writeTestFile(t, filepath.Join(stageDir, LastUpgradeVerFile), "v0.9.3")
	writeTestFile(t, filepath.Join(stageDir, HealthyMarkerFile), "v0.9.3")
	// worker 备份 + supervisor .old 都在（健康后都应清理）
	writeTestFile(t, filepath.Join(binDir, WorkerBinaryName+PreviousSuffix), "old-worker")
	supervisorPath := filepath.Join(binDir, SupervisorBinaryName)
	oldPath := supervisorPath + ".old"
	writeTestFile(t, supervisorPath, "new-supervisor")
	writeTestFile(t, oldPath, "good-old-supervisor")
	writeTestFile(t, filepath.Join(stageDir, SupervisorSelfSwapAwaitingHealthFile), "awaiting")

	m := NewMachine(stageDir, binDir, testLogger(), nil)
	m.selfPathResolver = func() (string, error) {
		return supervisorPath, nil
	}
	if err := m.HealthPoll(ctx, 50*time.Millisecond); err != nil {
		t.Fatalf("expected nil (healthy), got %v", err)
	}

	// .old 权威删除点：健康确认后同趟删除
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Error("supervisor .old 应在 HealthPoll 成功后被删除（权威删除点）")
	}
	// worker 备份同趟清理
	if _, err := os.Stat(filepath.Join(binDir, WorkerBinaryName+PreviousSuffix)); !os.IsNotExist(err) {
		t.Error(".previous 应同趟清理")
	}
	// awaiting sentinel 同点清除（既有语义）
	if _, err := os.Stat(filepath.Join(stageDir, SupervisorSelfSwapAwaitingHealthFile)); err == nil {
		t.Error("awaiting_health sentinel 应被清除")
	}
}

// TestMachine_HealthPoll_Success_ClearsAwaitingSentinel 验证 HealthPoll 健康确认路径
// 清除 supervisor_selfswap_awaiting_health sentinel（awaiting-health sentinel 生命周期右端点
// 的成功路径分支；超时 rollback 路径的清理在 superviseWorker，见 worker_test.go）。
func TestMachine_HealthPoll_Success_ClearsAwaitingSentinel(t *testing.T) {
	stageDir := t.TempDir()
	binDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 健康状态：lastVer == marker
	writeTestFile(t, filepath.Join(stageDir, LastUpgradeVerFile), "v1.0.0")
	writeTestFile(t, filepath.Join(stageDir, HealthyMarkerFile), "v1.0.0")
	writeTestFile(t, filepath.Join(binDir, WorkerBinaryName+PreviousSuffix), "old")
	// awaiting sentinel 存在（awaiting-health BootCheck 留下的）
	writeTestFile(t, filepath.Join(stageDir, SupervisorSelfSwapAwaitingHealthFile), "awaiting")

	m := NewMachine(stageDir, binDir, testLogger(), nil)
	err := m.HealthPoll(ctx, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("expected nil (healthy), got %v", err)
	}

	// awaiting sentinel 应被清除
	if _, err := os.Stat(filepath.Join(stageDir, SupervisorSelfSwapAwaitingHealthFile)); err == nil {
		t.Error("supervisor_selfswap_awaiting_health sentinel 应在 HealthPoll 成功后被清除")
	}
}
