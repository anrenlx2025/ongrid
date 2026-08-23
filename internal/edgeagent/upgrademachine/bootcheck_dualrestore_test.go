// bootcheck_dualrestore_test.go 测试：
// 健康超时双恢复 BootCheck 化 + awaiting_health 哨兵总年龄上限。
//
// 覆盖 BootCheck 步骤 1 的三个新语义：
//   - 中间态恢复：rollback.done + awaiting_health 共存 = worker 已回滚、supervisor
//     待恢复（superviseWorker 超时路径保留哨兵 + SCM 重启后到达这里）
//   - 年龄上限：awaiting_health 哨兵 mtime 年龄超上限 → 强制双恢复（先 worker
//     后 supervisor），彻底解决坏 supervisor 崩溃循环中每轮重启重新武装定时器、回滚
//     永不触发的漏洞
//   - healthy 残留哨兵：年龄超限但升级已确认健康 → 只清残留哨兵，不误回滚
//
// 断电中间态用预置文件组合模拟（同 converge_test.go 模式）；年龄用 os.Chtimes 预置 mtime。

package upgrademachine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// setMtime 把文件 atime/mtime 预置到指定时刻（模拟哨兵的武装时刻）。
func setMtime(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("Chtimes %s: %v", path, err)
	}
}

// --- awaiting_health 年龄超上限 → 强制双恢复 ---

// TestBootCheck_AwaitingHealthAgeExceeded_ForcesDualRollback 验证核心场景：
// 坏 supervisor 崩溃循环（每轮 SCM 重启重新武装 HealthPoll 定时器，180s deadline
// 永不到达）→ awaiting_health 哨兵 mtime（= selfswap 武装时刻，无人重写）随时间
// 累积超过上限 → BootCheck 强制双恢复：先 worker（.previous 回滚）后 supervisor
// （.old 改名回），返回 ErrSupervisorRestartSoon 让 SCM 自然重启加载旧 supervisor。
//
// 断言契约（磁盘终态 + 哨兵终态，不测内部调用序列）：
//   - 返回 ErrSupervisorRestartSoon（SCM restart 加载恢复后的旧 supervisor）
//   - worker.exe 内容 = 旧版（.previous 已恢复且清空）
//   - supervisor.exe 内容 = 旧版（.old 已恢复）；.old / .discard 均不存在
//   - awaiting_health 哨兵已清；rollback.done 已写（worker 回滚权威标记）
//   - PendingHealthCheck = false（不再 arm）
func TestBootCheck_AwaitingHealthAgeExceeded_ForcesDualRollback(t *testing.T) {
	stageDir := t.TempDir()
	binDir := t.TempDir()

	// 铺 worker 双版本 + supervisor 双版本（断电中间态文件组合）
	writeTestFile(t, filepath.Join(binDir, WorkerBinaryName), "new-bad-worker")
	writeTestFile(t, filepath.Join(binDir, WorkerBinaryName+PreviousSuffix), "good-old-worker")
	supervisorPath := filepath.Join(binDir, SupervisorBinaryName)
	writeTestFile(t, supervisorPath, "new-supervisor")
	writeTestFile(t, supervisorPath+".old", "good-old-supervisor")

	// 铺哨兵：awaiting_health 武装于 2h 前（超过 1h 上限）；meta 已写、健康标记缺失
	agingPath := filepath.Join(stageDir, SupervisorSelfSwapAwaitingHealthFile)
	writeTestFile(t, agingPath, "awaiting")
	setMtime(t, agingPath, time.Now().Add(-2*time.Hour))
	writeTestFile(t, filepath.Join(stageDir, LastUpgradeVerFile), "v0.9.4")

	var pc mockProcessController
	m := NewMachine(stageDir, binDir, testLogger(), &pc)
	m.selfPathResolver = func() (string, error) {
		return supervisorPath, nil
	}
	m.awaitingHealthMaxAge = 1 * time.Hour

	err := m.BootCheck(context.Background())
	if !errors.Is(err, ErrSupervisorRestartSoon) {
		t.Fatalf("期望 ErrSupervisorRestartSoon（强制双恢复后 SCM restart），got %v", err)
	}

	// worker 已回滚（先恢复）
	got, rerr := os.ReadFile(filepath.Join(binDir, WorkerBinaryName))
	if rerr != nil || string(got) != "good-old-worker" {
		t.Errorf("worker.exe 应回滚到旧版，got %q (err=%v)", got, rerr)
	}
	if _, err := os.Stat(filepath.Join(binDir, WorkerBinaryName+PreviousSuffix)); !os.IsNotExist(err) {
		t.Errorf(".previous 应已恢复清空（err=%v）", err)
	}
	// supervisor 已恢复（后恢复）
	got, rerr = os.ReadFile(supervisorPath)
	if rerr != nil || string(got) != "good-old-supervisor" {
		t.Errorf("supervisor.exe 应恢复为旧版，got %q (err=%v)", got, rerr)
	}
	if _, err := os.Stat(supervisorPath + ".old"); !os.IsNotExist(err) {
		t.Errorf(".old 应已被 rename 走（err=%v）", err)
	}
	if _, err := os.Stat(supervisorPath + supervisorDiscardSuffix); !os.IsNotExist(err) {
		t.Errorf(".discard（新 exe aside 残留）应被清理（err=%v）", err)
	}
	// 哨兵终态
	if IsSupervisorSelfSwapAwaitingHealth(stageDir) {
		t.Errorf("awaiting_health 哨兵应被清除")
	}
	if !RollbackDoneExists(stageDir) {
		t.Errorf("rollback.done 应已写入（worker 回滚权威标记）")
	}
	if m.PendingHealthCheck() {
		t.Errorf("PendingHealthCheck() 应为 false（已强制回滚，不再 arm）")
	}
	// 崩溃循环场景 orphan worker 持 worker.exe 锁会让 Rollback 失败 —
	// 恢复前先 KillByImage 清 orphan（对称 BootCheck 步骤 5 的 KillByImage 先例）
	if pc.killImageCalls.Load() != 1 || len(pc.killImageNames) != 1 ||
		pc.killImageNames[0] != WorkerBinaryName {
		t.Errorf("KillByImage(worker) 应被调用 1 次，got calls=%d names=%v",
			pc.killImageCalls.Load(), pc.killImageNames)
	}
}

// TestBootCheck_AwaitingHealthAgeFresh_ArmsHealthPoll_NoRollback 验证年龄未超上限时
// 保持 awaiting-health 语义：arm HealthPoll 180s grace，不提前回滚。
func TestBootCheck_AwaitingHealthAgeFresh_ArmsHealthPoll_NoRollback(t *testing.T) {
	stageDir := t.TempDir()
	binDir := t.TempDir()

	writeTestFile(t, filepath.Join(binDir, WorkerBinaryName), "new-worker")
	supervisorPath := filepath.Join(binDir, SupervisorBinaryName)
	writeTestFile(t, supervisorPath, "new-supervisor")
	writeTestFile(t, supervisorPath+".old", "good-old-supervisor")

	agingPath := filepath.Join(stageDir, SupervisorSelfSwapAwaitingHealthFile)
	writeTestFile(t, agingPath, "awaiting")
	setMtime(t, agingPath, time.Now().Add(-30*time.Minute)) // 新于 1h 上限
	writeTestFile(t, filepath.Join(stageDir, LastUpgradeVerFile), "v0.9.4")

	m := NewMachine(stageDir, binDir, testLogger(), nil)
	m.selfPathResolver = func() (string, error) {
		return supervisorPath, nil
	}
	m.awaitingHealthMaxAge = 1 * time.Hour

	if err := m.BootCheck(context.Background()); err != nil {
		t.Fatalf("BootCheck 不应返回错误（arm 语义）： %v", err)
	}

	if !m.PendingHealthCheck() {
		t.Errorf("PendingHealthCheck() 应为 true（awaiting-health 语义保留）")
	}
	// 无任何回滚/恢复发生
	got, _ := os.ReadFile(filepath.Join(binDir, WorkerBinaryName))
	if string(got) != "new-worker" {
		t.Errorf("worker.exe 不应被改动，got %q", got)
	}
	got, _ = os.ReadFile(supervisorPath)
	if string(got) != "new-supervisor" {
		t.Errorf("supervisor.exe 不应被改动，got %q", got)
	}
	if _, err := os.Stat(supervisorPath + ".old"); err != nil {
		t.Errorf(".old 应存活（err=%v）", err)
	}
	if RollbackDoneExists(stageDir) {
		t.Errorf("不应写 rollback.done")
	}
}

// --- 中间态恢复：rollback.done + awaiting_health 共存 ---

// TestBootCheck_RollbackDonePlusAwaiting_RestoresSupervisorOnly 验证双恢复的
// 中间态信号：superviseWorker 超时路径已完成 worker 回滚（rollback.done 已写）并
// 保留 awaiting_health 哨兵 + SCM 重启 → BootCheck 检测组合态 → 只补 supervisor
// 侧恢复（.old 改名回），返回 ErrSupervisorRestartSoon。
//
// 断电推演：中间点（worker 已回滚 / supervisor 未恢复）不留版本混合态 — 任意点
// 断电后下次 BootCheck 都到达同一恢复路径（rollback.done + awaiting 组合幂等）。
func TestBootCheck_RollbackDonePlusAwaiting_RestoresSupervisorOnly(t *testing.T) {
	stageDir := t.TempDir()
	binDir := t.TempDir()

	// worker 已回滚（superviseWorker 上辈子完成）：旧版 + 无 .previous
	writeTestFile(t, filepath.Join(binDir, WorkerBinaryName), "good-old-worker")
	supervisorPath := filepath.Join(binDir, SupervisorBinaryName)
	writeTestFile(t, supervisorPath, "new-supervisor")
	writeTestFile(t, supervisorPath+".old", "good-old-supervisor")

	// rollback.done + awaiting_health 共存（superviseWorker 超时路径的落盘终态）。
	//  mtime 判据：真中间态 rollback.done 必晚于 awaiting（先武装 180s 超时
	// 后才写 rollback.done），锚定真序防判据误伤。
	writeTestFile(t, filepath.Join(stageDir, RollbackDoneFile), "done")
	agingPath := filepath.Join(stageDir, SupervisorSelfSwapAwaitingHealthFile)
	writeTestFile(t, agingPath, "awaiting")
	now := time.Now()
	setMtime(t, agingPath, now.Add(-35*time.Minute))
	setMtime(t, filepath.Join(stageDir, RollbackDoneFile), now.Add(-30*time.Minute))

	m := NewMachine(stageDir, binDir, testLogger(), nil)
	m.selfPathResolver = func() (string, error) {
		return supervisorPath, nil
	}
	m.awaitingHealthMaxAge = 1 * time.Hour

	err := m.BootCheck(context.Background())
	if !errors.Is(err, ErrSupervisorRestartSoon) {
		t.Fatalf("期望 ErrSupervisorRestartSoon（supervisor 侧恢复 + SCM restart），got %v", err)
	}

	// supervisor 恢复为旧版；aside 残留清理
	got, rerr := os.ReadFile(supervisorPath)
	if rerr != nil || string(got) != "good-old-supervisor" {
		t.Errorf("supervisor.exe 应恢复为旧版，got %q (err=%v)", got, rerr)
	}
	if _, err := os.Stat(supervisorPath + ".old"); !os.IsNotExist(err) {
		t.Errorf(".old 应已被 rename 走（err=%v）", err)
	}
	if _, err := os.Stat(supervisorPath + supervisorDiscardSuffix); !os.IsNotExist(err) {
		t.Errorf(".discard 应被清理（err=%v）", err)
	}
	// worker 不再动（已回滚）
	got, _ = os.ReadFile(filepath.Join(binDir, WorkerBinaryName))
	if string(got) != "good-old-worker" {
		t.Errorf("worker.exe 不应被改动，got %q", got)
	}
	// awaiting_health 已清；不再 arm
	if IsSupervisorSelfSwapAwaitingHealth(stageDir) {
		t.Errorf("awaiting_health 哨兵应被清除")
	}
	if m.PendingHealthCheck() {
		t.Errorf("PendingHealthCheck() 应为 false")
	}
}

// TestBootCheck_RollbackDonePlusAwaiting_NoOld_ClearsSentinelAndContinues 验证
// 中间态但 .old 缺失（如健康路径清理已跑、仅哨兵删除失败的自愈场景）：无处恢复 →
// 只清残留哨兵，正常继续启动（不 restart）。
func TestBootCheck_RollbackDonePlusAwaiting_NoOld_ClearsSentinelAndContinues(t *testing.T) {
	stageDir := t.TempDir()
	binDir := t.TempDir()

	writeTestFile(t, filepath.Join(binDir, WorkerBinaryName), "good-old-worker")
	supervisorPath := filepath.Join(binDir, SupervisorBinaryName)
	writeTestFile(t, supervisorPath, "supervisor-current") // 无 .old

	writeTestFile(t, filepath.Join(stageDir, RollbackDoneFile), "done")
	writeTestFile(t, filepath.Join(stageDir, SupervisorSelfSwapAwaitingHealthFile), "awaiting")
	//  mtime 判据：锚定真序（rollback.done 晚于 awaiting），否则同秒写入顺序不定
	setMtime(t, filepath.Join(stageDir, SupervisorSelfSwapAwaitingHealthFile), time.Now().Add(-35*time.Minute))
	setMtime(t, filepath.Join(stageDir, RollbackDoneFile), time.Now().Add(-30*time.Minute))

	m := NewMachine(stageDir, binDir, testLogger(), nil)
	m.selfPathResolver = func() (string, error) {
		return supervisorPath, nil
	}

	err := m.BootCheck(context.Background())
	if errors.Is(err, ErrSupervisorRestartSoon) {
		t.Fatalf("无 .old 可恢复时不应触发 SCM restart")
	}
	if err != nil {
		t.Fatalf("BootCheck 应干净返回 nil： %v", err)
	}

	if IsSupervisorSelfSwapAwaitingHealth(stageDir) {
		t.Errorf("残留 awaiting_health 哨兵应被清除（自愈）")
	}
	got, _ := os.ReadFile(supervisorPath)
	if string(got) != "supervisor-current" {
		t.Errorf("supervisor.exe 不应被改动，got %q", got)
	}
	if m.PendingHealthCheck() {
		t.Errorf("PendingHealthCheck() 应为 false")
	}
}

// TestBootCheck_RollbackDonePlusAwaiting_BrickState_RestoresWithoutAside 验证
// brick 态双恢复边界：supervisor.exe 缺失（如恢复趟 rename aside 后断电）+ .old
// 存在 + rollback.done + awaiting_health 共存 → restoreSupervisorFromOld 跳过
// aside（无 exe 可挪）直接 .old rename 回，锚定该 Stat 分支防未来重构回归。
func TestBootCheck_RollbackDonePlusAwaiting_BrickState_RestoresWithoutAside(t *testing.T) {
	stageDir := t.TempDir()
	binDir := t.TempDir()

	writeTestFile(t, filepath.Join(binDir, WorkerBinaryName), "good-old-worker")
	supervisorPath := filepath.Join(binDir, SupervisorBinaryName)
	// brick 态：supervisor.exe 缺失，仅 .old 存在
	writeTestFile(t, supervisorPath+".old", "good-old-supervisor")

	writeTestFile(t, filepath.Join(stageDir, RollbackDoneFile), "done")
	writeTestFile(t, filepath.Join(stageDir, SupervisorSelfSwapAwaitingHealthFile), "awaiting")
	//  mtime 判据：锚定真序（rollback.done 晚于 awaiting），否则同秒写入顺序不定
	setMtime(t, filepath.Join(stageDir, SupervisorSelfSwapAwaitingHealthFile), time.Now().Add(-35*time.Minute))
	setMtime(t, filepath.Join(stageDir, RollbackDoneFile), time.Now().Add(-30*time.Minute))

	m := NewMachine(stageDir, binDir, testLogger(), nil)
	m.selfPathResolver = func() (string, error) {
		return supervisorPath, nil
	}

	// 注意：此场景步骤 4 brick recovery 与步骤 1 中间态恢复都会尝试 rename 回，
	// 无论哪个执行，磁盘终态一致（幂等）。断言终态即可。
	err := m.BootCheck(context.Background())
	if !errors.Is(err, ErrSupervisorRestartSoon) {
		t.Fatalf("期望 ErrSupervisorRestartSoon（supervisor 恢复 + SCM restart），got %v", err)
	}

	got, rerr := os.ReadFile(supervisorPath)
	if rerr != nil || string(got) != "good-old-supervisor" {
		t.Errorf("supervisor.exe 应恢复为旧版，got %q (err=%v)", got, rerr)
	}
	if _, serr := os.Stat(supervisorPath + ".old"); !os.IsNotExist(serr) {
		t.Errorf(".old 应已被 rename 走（err=%v）", serr)
	}
	if _, derr := os.Stat(supervisorPath + supervisorDiscardSuffix); !os.IsNotExist(derr) {
		t.Errorf(".discard 不应被创建（无 aside 需要，err=%v）", derr)
	}
}

// --- healthy 残留哨兵：不误回滚 ---

// TestBootCheck_AwaitingHealthAgeExceededButHealthy_ClearsStaleSentinel 验证：
// HealthPoll 成功路径清 awaiting_health 失败（best-effort）+ 长期运行后重启 →
// 哨兵年龄超限但升级已确认健康 → 只清残留哨兵，绝不回滚健康升级。
func TestBootCheck_AwaitingHealthAgeExceededButHealthy_ClearsStaleSentinel(t *testing.T) {
	stageDir := t.TempDir()
	binDir := t.TempDir()

	writeTestFile(t, filepath.Join(binDir, WorkerBinaryName), "new-healthy-worker")
	supervisorPath := filepath.Join(binDir, SupervisorBinaryName)
	writeTestFile(t, supervisorPath, "new-supervisor")
	writeTestFile(t, supervisorPath+".old", "good-old-supervisor")

	agingPath := filepath.Join(stageDir, SupervisorSelfSwapAwaitingHealthFile)
	writeTestFile(t, agingPath, "awaiting")
	setMtime(t, agingPath, time.Now().Add(-48*time.Hour)) // 远超上限
	// 健康已确认：healthy_marker == last_upgrade_ver
	writeTestFile(t, filepath.Join(stageDir, LastUpgradeVerFile), "v0.9.4")
	writeTestFile(t, filepath.Join(stageDir, HealthyMarkerFile), "v0.9.4")
	WriteSupervisorUpgradeApplied(stageDir, "v0.9.4")

	m := NewMachine(stageDir, binDir, testLogger(), nil)
	m.selfPathResolver = func() (string, error) {
		return supervisorPath, nil
	}
	m.awaitingHealthMaxAge = 1 * time.Hour

	if err := m.BootCheck(context.Background()); err != nil {
		t.Fatalf("BootCheck 应干净返回 nil： %v", err)
	}

	if IsSupervisorSelfSwapAwaitingHealth(stageDir) {
		t.Errorf("残留哨兵应被清除")
	}
	// 健康升级不回滚：双 binary 均保持新版
	got, _ := os.ReadFile(filepath.Join(binDir, WorkerBinaryName))
	if string(got) != "new-healthy-worker" {
		t.Errorf("健康升级的 worker 不应被回滚，got %q", got)
	}
	got, _ = os.ReadFile(supervisorPath)
	if string(got) != "new-supervisor" {
		t.Errorf("健康升级的 supervisor 不应被回滚，got %q", got)
	}
	// applied + 健康态 → 步骤 3 backstop 清 .old（幂等语义保留）
	if _, err := os.Stat(supervisorPath + ".old"); !os.IsNotExist(err) {
		t.Errorf(".old 应被步骤 3 backstop 清理（err=%v）", err)
	}
	if RollbackDoneExists(stageDir) {
		t.Errorf("不应写 rollback.done")
	}
	if m.PendingHealthCheck() {
		t.Errorf("PendingHealthCheck() 应为 false（升级已结束）")
	}
}

// --- 年龄判定边界 ---

// TestAwaitingHealthAgeExceeded_Boundary 验证年龄判定纯语义：
// 超限是严格大于（恰好等于不触发）；哨兵缺失返回 false（防御）。
func TestAwaitingHealthAgeExceeded_Boundary(t *testing.T) {
	stageDir := t.TempDir()
	m := NewMachine(stageDir, t.TempDir(), testLogger(), nil)
	m.awaitingHealthMaxAge = 1 * time.Hour
	now := time.Now()

	// 哨兵缺失 → false
	if m.awaitingHealthAgeExceeded(now) {
		t.Errorf("哨兵缺失时应返回 false")
	}

	agingPath := filepath.Join(stageDir, SupervisorSelfSwapAwaitingHealthFile)
	writeTestFile(t, agingPath, "awaiting")

	// 恰好等于上限 → 不超限（严格大于）
	setMtime(t, agingPath, now.Add(-1*time.Hour))
	if m.awaitingHealthAgeExceeded(now) {
		t.Errorf("年龄恰好等于上限不应触发（严格大于语义）")
	}
	// 超过上限 → true
	setMtime(t, agingPath, now.Add(-1*time.Hour-1*time.Minute))
	if !m.awaitingHealthAgeExceeded(now) {
		t.Errorf("年龄超过上限应触发")
	}
}

// --- 共存分支 mtime 顺序判据 ---

// TestBootCheck_Coexistence_MtimeOrderDiscriminates 验证：rollback.done 与
// awaiting_health 共存只看共存会误伤 — 实机现场是 7 天前旧周期
// rollback.done 残留 + 本周期 SelfSwap 刚写的 awaiting_health 同形假阳性，
// 把健康的刚 selfswap supervisor 误回滚成旧版。
//
// 判据：真中间态时序 = 先武装（写 awaiting_health）→ 健康超时 → 写 rollback.done，
// 故 rollback.done.mtime 必严格晚于 awaiting_health.mtime；早于/相等 = stale
// rollback.done 残留（清哨兵，按 awaiting-only 语义继续，不回滚）。
func TestBootCheck_Coexistence_MtimeOrderDiscriminates(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		// 哨兵 mtime 偏移（相对 now，负值=过去）
		rollbackDoneOffset time.Duration
		awaitingOffset     time.Duration
		wantRestore        bool // true = 真中间态（双恢复 supervisor）
	}{
		{
			name:               "真中间态：rollback.done 晚于 awaiting → 双恢复",
			rollbackDoneOffset: -30 * time.Minute,
			awaitingOffset:     -35 * time.Minute,
			wantRestore:        true,
		},
		{
			name:               "stale：rollback.done 早于 awaiting（前周期残留现场）→ 不回滚",
			rollbackDoneOffset: -7 * 24 * time.Hour,
			awaitingOffset:     -30 * time.Minute,
			wantRestore:        false,
		},
		{
			name:               "边界：mtime 相等 → stale（真中间态两哨兵至少隔 180s 不可能相等）",
			rollbackDoneOffset: -30 * time.Minute,
			awaitingOffset:     -30 * time.Minute,
			wantRestore:        false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stageDir := t.TempDir()
			binDir := t.TempDir()

			// 刚 selfswap 的健康新版组合：supervisor 新版 + .old 备份，worker 新版
			writeTestFile(t, filepath.Join(binDir, WorkerBinaryName), "new-worker")
			supervisorPath := filepath.Join(binDir, SupervisorBinaryName)
			writeTestFile(t, supervisorPath, "new-supervisor")
			writeTestFile(t, supervisorPath+".old", "good-old-supervisor")

			writeTestFile(t, filepath.Join(stageDir, RollbackDoneFile), "done")
			awaitingPath := filepath.Join(stageDir, SupervisorSelfSwapAwaitingHealthFile)
			writeTestFile(t, awaitingPath, "awaiting")
			setMtime(t, filepath.Join(stageDir, RollbackDoneFile), now.Add(tc.rollbackDoneOffset))
			setMtime(t, awaitingPath, now.Add(tc.awaitingOffset))

			m := NewMachine(stageDir, binDir, testLogger(), nil)
			m.selfPathResolver = func() (string, error) {
				return supervisorPath, nil
			}
			m.awaitingHealthMaxAge = 1 * time.Hour // awaiting 均新鲜 → 不会走强制双恢复分支

			err := m.BootCheck(context.Background())

			if tc.wantRestore {
				// 真中间态：补 supervisor 恢复 + SCM restart（既有语义不变）
				if !errors.Is(err, ErrSupervisorRestartSoon) {
					t.Fatalf("期望 ErrSupervisorRestartSoon（真中间态双恢复），got %v", err)
				}
				got, rerr := os.ReadFile(supervisorPath)
				if rerr != nil || string(got) != "good-old-supervisor" {
					t.Errorf("supervisor.exe 应恢复为旧版，got %q (err=%v)", got, rerr)
				}
				if m.PendingHealthCheck() {
					t.Errorf("真中间态不应 arm HealthPoll")
				}
			} else {
				// stale：不回滚刚 selfswap 的健康 supervisor，清残留哨兵，
				// 按 awaiting-only 语义 arm HealthPoll（awaiting-health）
				if err != nil {
					t.Fatalf("stale 组合不应触发错误/重启： %v", err)
				}
				got, rerr := os.ReadFile(supervisorPath)
				if rerr != nil || string(got) != "new-supervisor" {
					t.Errorf("健康的新版 supervisor 不应被回滚，got %q (err=%v)", got, rerr)
				}
				if _, serr := os.Stat(supervisorPath + ".old"); serr != nil {
					t.Errorf(".old 应存活（升级仍在健康确认中，err=%v）", serr)
				}
				if RollbackDoneExists(stageDir) {
					t.Errorf("stale rollback.done 应被清除")
				}
				if !IsSupervisorSelfSwapAwaitingHealth(stageDir) {
					t.Errorf("awaiting_health 应保留（awaiting-health 生命周期右端点在 HealthPoll）")
				}
				if !m.PendingHealthCheck() {
					t.Errorf("stale 清除后应按 awaiting-only 语义 arm HealthPoll（awaiting-health）")
				}
			}
		})
	}
}
