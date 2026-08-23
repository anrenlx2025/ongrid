// bootcheck_stale_applied_test.go 测试 BootCheck 步骤 3 的  孤儿
// pending 清理遇跨周期混叠（stale applied + 新周期 pending）不得误清。
//
// 与 （rollbackDoneAfterAwaitingHealth）同族的 mtime 顺序判据：
// 真孤儿时序 = Apply 先写 pending → SelfSwap 后写 applied，故
// pending.mtime 必早于 applied.mtime；新周期= 上周期 SelfSwap
// 写 applied → SCM restart → 新 Apply 写 pending，故 pending.mtime 必晚于
// applied.mtime（跨 restart ≥ 秒级）。早于/相等 = 孤儿（清）；严格晚于 =
// 新周期（不清，留给步骤 5 SelfSwap 正常消费）。
//
// 断电中间态用预置文件组合模拟（同 / 模式）；mtime 用 setMtime 锚定真序。

package upgrademachine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBootCheck_OrphanPendingMtimeOrder 验证：applied + pending 共存时
// 步骤 3 用 mtime 顺序区分孤儿与新周期 — 实机现场是上周期 applied 残留 +
// 本周期 Apply 刚写的 pending 同形假阳性，无条件清会把新周期 pending 误删，
// 步骤 5 SelfSwap 不触发，supervisor.exe.new 永久残留（本轮升级丢失）。
func TestBootCheck_OrphanPendingMtimeOrder(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name          string
		appliedOffset time.Duration
		pendingOffset time.Duration
		wantRestart   bool // true = 新周期：不误清，步骤 5 SelfSwap 触发
	}{
		{
			name:          "新周期：pending 晚于 applied（跨周期新升级）→ 不清，SelfSwap 触发",
			appliedOffset: -10 * time.Minute,
			pendingOffset: -1 * time.Minute,
			wantRestart:   true,
		},
		{
			name:          "真孤儿：pending 早于 applied（断电窗口）→ 清理，不触发 SelfSwap",
			appliedOffset: -1 * time.Minute,
			pendingOffset: -10 * time.Minute,
			wantRestart:   false,
		},
		{
			name:          "边界：mtime 相等 → 判孤儿（真孤儿两哨兵至少隔一次 SelfSwap 不可能相等，相等=stale 语义一致）",
			appliedOffset: -10 * time.Minute,
			pendingOffset: -10 * time.Minute,
			wantRestart:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dummy := buildDummy(t)
			stageDir := t.TempDir()
			binDir := t.TempDir()

			// 铺 supervisor.exe（已是新版 — 上次 swap 成功，/ 共同前置）
			supervisorPath := filepath.Join(binDir, SupervisorBinaryName)
			copyFileExe(t, dummy, supervisorPath)
			appendMarker(t, supervisorPath, "CURRENT-SUPERVISOR\n")
			// 铺 .old 备份（上周期 swap step 1 残留）
			copyFileExe(t, dummy, supervisorPath+".old")

			// 铺 applied + pending 哨兵，锚定 mtime 真序（对称  测试）
			appliedPath := SupervisorUpgradeAppliedPath(stageDir)
			pendingPath := SupervisorUpgradePendingPath(stageDir)
			writeTestFile(t, appliedPath, "applied")
			writeTestFile(t, pendingPath, "pending")
			setMtime(t, appliedPath, now.Add(tc.appliedOffset))
			setMtime(t, pendingPath, now.Add(tc.pendingOffset))

			// 新周期场景：.new 已 stage（本周期 Apply 刚写的，等步骤 5 消费）
			if tc.wantRestart {
				copyFileExe(t, dummy, supervisorPath+".new")
				appendMarker(t, supervisorPath+".new", "NEW-SUPERVISOR\n")
			}

			m := NewMachine(stageDir, binDir, testLogger(), nil)
			m.selfPathResolver = func() (string, error) {
				return supervisorPath, nil
			}

			err := m.BootCheck(context.Background())

			if tc.wantRestart {
				// 新周期：pending 不被误清 → 步骤 5 SelfSwap 消费 .new
				if !errors.Is(err, ErrSupervisorRestartSoon) {
					t.Fatalf("期望 ErrSupervisorRestartSoon（SelfSwap 应触发），got %v", err)
				}
				got, rerr := os.ReadFile(supervisorPath)
				if rerr != nil || !contains(got, "NEW-SUPERVISOR") {
					t.Errorf("supervisor.exe 应已被 .new 换新，got %q (err=%v)", got, rerr)
				}
				if _, serr := os.Stat(supervisorPath + ".new"); !os.IsNotExist(serr) {
					t.Errorf(".new 应被 SelfSwap 消费（err=%v）", serr)
				}
				// SelfSwap step 3 写了新 applied（swap 成功的佐证）；
				// stale applied 已被步骤 3 先行删除（新写的 mtime 更晚）
				if !IsSupervisorUpgradeApplied(stageDir) {
					t.Errorf("SelfSwap 完成后应写新 applied sentinel")
				}
			} else {
				// 孤儿：pending 被清 + 不触发 SelfSwap
				if err != nil {
					t.Fatalf("孤儿组合应干净返回 nil： %v", err)
				}
				if IsSupervisorUpgradePending(stageDir) {
					t.Errorf("孤儿 pending 应被清理（孤儿判定语义不变）")
				}
				// 步骤 3 删 applied 后无人重写（不走 SelfSwap）
				if IsSupervisorUpgradeApplied(stageDir) {
					t.Errorf("applied sentinel 应被删除（收尾语义保留）")
				}
				got, rerr := os.ReadFile(supervisorPath)
				if rerr != nil || !contains(got, "CURRENT-SUPERVISOR") {
					t.Errorf("supervisor.exe 不应被改动，got %q (err=%v)", got, rerr)
				}
			}
		})
	}
}

// TestBootCheck_RefeedWithStaleApplied 验证  票验收第 1 条的代码级等价：
// 重投喂 bundle + applied 残留窗口的完整 BootCheck 时序 —
// 步骤 2 Apply 写新 pending（mtime=now）→ 步骤 3 读到上周期 stale applied
// （mtime 过去）→ 不误清 → 步骤 5 SelfSwap 正常触发，.new 被消费无残留。
//
// 事故时序（修复前）：步骤 3 无条件清新周期 pending → 步骤 5 不触发 →
// supervisor.exe.new 永久残留。
func TestBootCheck_RefeedWithStaleApplied(t *testing.T) {
	dummy := buildDummy(t)
	stageDir := t.TempDir()
	binDir := t.TempDir()

	// 当前 supervisor（旧版）
	supervisorPath := filepath.Join(binDir, SupervisorBinaryName)
	copyFileExe(t, dummy, supervisorPath)
	appendMarker(t, supervisorPath, "OLD-SUPERVISOR\n")

	// 新投喂 bundle：incoming/ 新 supervisor + MANIFEST + VERSION
	incoming := filepath.Join(stageDir, IncomingDirName)
	if err := os.MkdirAll(incoming, 0o755); err != nil {
		t.Fatalf("mkdir incoming: %v", err)
	}
	srcExe := filepath.Join(incoming, SupervisorBinaryName)
	copyFileExe(t, dummy, srcExe)
	appendMarker(t, srcExe, "NEW-SUPERVISOR\n")
	manifestLine := sha256Hex(t, srcExe) + " 0755 " + SupervisorBinaryName + " " + supervisorPath + "\n"
	writeTestFile(t, filepath.Join(incoming, ManifestFileName), manifestLine)
	writeTestFile(t, filepath.Join(incoming, VersionFileName), "v0.9.4-test121\n")

	// 上周期残留的 applied 哨兵（mtime 已过去 10 分钟）
	appliedPath := SupervisorUpgradeAppliedPath(stageDir)
	writeTestFile(t, appliedPath, "applied")
	setMtime(t, appliedPath, time.Now().Add(-10*time.Minute))

	m := NewMachine(stageDir, binDir, testLogger(), nil)
	m.selfPathResolver = func() (string, error) {
		return supervisorPath, nil
	}

	err := m.BootCheck(context.Background())

	// 核心断言：SelfSwap 正常触发（新周期 pending 未被步骤 3 误清）
	if !errors.Is(err, ErrSupervisorRestartSoon) {
		t.Fatalf("期望 ErrSupervisorRestartSoon（重投喂 SelfSwap 应触发），got %v", err)
	}
	got, rerr := os.ReadFile(supervisorPath)
	if rerr != nil || !contains(got, "NEW-SUPERVISOR") {
		t.Errorf("supervisor.exe 应被换新，got %q (err=%v)", got, rerr)
	}
	if _, serr := os.Stat(supervisorPath + ".new"); !os.IsNotExist(serr) {
		t.Errorf(".new 应被消费无残留（err=%v）", serr)
	}
	if IsSupervisorUpgradePending(stageDir) {
		t.Errorf("pending 应被 SelfSwap 收尾清理")
	}
}

// contains 是 strings.Contains 的字节切片便捷形式（测试断言用）。
func contains(b []byte, sub string) bool {
	return strings.Contains(string(b), sub)
}

// sha256Hex 计算文件 sha256 hex（跨平台 MANIFEST 行构造用；
// apply_cycle_windows_test.go 的 shaFile 是 windows-only）。
func sha256Hex(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
