// converge_test.go 验证：apply 入口残留收敛。
//
// 断电组合态用预置文件组合模拟（测试决策：不引入故障注入框架）—
// 每个用例对应一类断电死锁场景：
//   - apply 半换断电 + 健康标记匹配旧值 → 回滚不可达死锁
//   - 多条目半换断电（一条已换、一条备份未改名）
//   - 备份后改名前断电 + supervisor 暂存残留（.new + pending 哨兵）
//   - 健康标记假阳性（WriteUpgradeMeta 未达）+ supervisor 暂存残留 + 旧哨兵

package upgrademachine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// convergeCase 是断电组合态表驱动用例。
type convergeCase struct {
	name string
	// preset 在 binDir/stageDir 预置断电后的文件组合
	preset func(t *testing.T, stageDir, binDir string)
	// verify 断言收敛后的磁盘/哨兵终态
	verify func(t *testing.T, stageDir, binDir string, restored int, err error)
}

var convergeCases = []convergeCase{
	{
		name: "半换断电_健康标记匹配旧值死锁态",
		preset: func(t *testing.T, stageDir, binDir string) {
			// apply 在 worker swap 后、WriteUpgradeMeta 前断电：
			// last_upgrade_ver 仍是上一健康版本 V1，healthy_marker=V1 →
			// IsUpgradeHealthy=true → 回滚永不触发 + .previous 残留
			writeTestFile(t, filepath.Join(binDir, WorkerBinaryName), "V2-broken")
			writeTestFile(t, filepath.Join(binDir, WorkerBinaryName+PreviousSuffix), "V1-old")
			writeTestFile(t, filepath.Join(stageDir, LastUpgradeVerFile), "V1")
			writeTestFile(t, filepath.Join(stageDir, HealthyMarkerFile), "V1")
		},
		verify: func(t *testing.T, stageDir, binDir string, restored int, err error) {
			if err != nil {
				t.Fatalf("converge: %v", err)
			}
			if restored != 1 {
				t.Errorf("restored = %d, want 1", restored)
			}
			got, _ := os.ReadFile(filepath.Join(binDir, WorkerBinaryName))
			if string(got) != "V1-old" {
				t.Errorf("worker.exe = %q, want %q (converged to old)", got, "V1-old")
			}
			if _, statErr := os.Stat(filepath.Join(binDir, WorkerBinaryName+PreviousSuffix)); !os.IsNotExist(statErr) {
				t.Errorf(".previous must be consumed")
			}
		},
	},
	{
		name: "多条目半换断电_已换与未换并存",
		preset: func(t *testing.T, stageDir, binDir string) {
			// worker 已 swap（dest=V2），exporter 仅备份未改名（dest=V1）
			writeTestFile(t, filepath.Join(binDir, WorkerBinaryName), "V2-worker")
			writeTestFile(t, filepath.Join(binDir, WorkerBinaryName+PreviousSuffix), "V1-worker")
			writeTestFile(t, filepath.Join(binDir, "windows_exporter.exe"), "V1-exporter")
			writeTestFile(t, filepath.Join(binDir, "windows_exporter.exe"+PreviousSuffix), "V1-exporter")
		},
		verify: func(t *testing.T, stageDir, binDir string, restored int, err error) {
			if err != nil {
				t.Fatalf("converge: %v", err)
			}
			if restored != 2 {
				t.Errorf("restored = %d, want 2", restored)
			}
			// 恢复集必须含未换条目（非仅已改名集）
			for name, want := range map[string]string{
				WorkerBinaryName:       "V1-worker",
				"windows_exporter.exe": "V1-exporter",
			} {
				got, _ := os.ReadFile(filepath.Join(binDir, name))
				if string(got) != want {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
				if _, statErr := os.Stat(filepath.Join(binDir, name+PreviousSuffix)); !os.IsNotExist(statErr) {
					t.Errorf("%s.previous must be consumed", name)
				}
			}
		},
	},
	{
		name: "备份后改名前断电_supervisor暂存残留",
		preset: func(t *testing.T, stageDir, binDir string) {
			// worker 仅备份未改名 + supervisor .new 已 stage + pending 哨兵已写
			writeTestFile(t, filepath.Join(binDir, WorkerBinaryName), "V1-worker")
			writeTestFile(t, filepath.Join(binDir, WorkerBinaryName+PreviousSuffix), "V1-worker")
			writeTestFile(t, filepath.Join(binDir, SupervisorBinaryName), "V1-supervisor")
			writeTestFile(t, filepath.Join(binDir, SupervisorBinaryName+".new"), "V2-supervisor-stale")
			writeTestFile(t, filepath.Join(stageDir, SupervisorUpgradePendingFile), "V2")
		},
		verify: func(t *testing.T, stageDir, binDir string, restored int, err error) {
			if err != nil {
				t.Fatalf("converge: %v", err)
			}
			if _, statErr := os.Stat(filepath.Join(binDir, WorkerBinaryName+PreviousSuffix)); !os.IsNotExist(statErr) {
				t.Errorf(".previous must be consumed")
			}
			// supervisor 暂存 .new + pending 哨兵必须清理（防越权 self-swap）
			if _, statErr := os.Stat(filepath.Join(binDir, SupervisorBinaryName+".new")); !os.IsNotExist(statErr) {
				t.Errorf("stale supervisor .new must be cleaned")
			}
			if IsSupervisorUpgradePending(stageDir) {
				t.Errorf("pending sentinel must be cleaned")
			}
			// supervisor.exe 本体不动（rename-aside 是 self-swap 的职责，非收敛）
			got, _ := os.ReadFile(filepath.Join(binDir, SupervisorBinaryName))
			if string(got) != "V1-supervisor" {
				t.Errorf("supervisor.exe = %q, want untouched %q", got, "V1-supervisor")
			}
		},
	},
	{
		name: "健康标记假阳性_暂存残留_旧回滚哨兵",
		preset: func(t *testing.T, stageDir, binDir string) {
			// 健康标记假阳性态 + supervisor 暂存残留 + 前一周期回滚遗留 rollback.done
			writeTestFile(t, filepath.Join(binDir, WorkerBinaryName), "V2-broken")
			writeTestFile(t, filepath.Join(binDir, WorkerBinaryName+PreviousSuffix), "V1-old")
			writeTestFile(t, filepath.Join(stageDir, LastUpgradeVerFile), "V1")
			writeTestFile(t, filepath.Join(stageDir, HealthyMarkerFile), "V1")
			writeTestFile(t, filepath.Join(binDir, SupervisorBinaryName), "V1-supervisor")
			writeTestFile(t, filepath.Join(binDir, SupervisorBinaryName+".new"), "V2-supervisor-stale")
			writeTestFile(t, filepath.Join(stageDir, SupervisorUpgradePendingFile), "V2")
			writeTestFile(t, filepath.Join(stageDir, RollbackDoneFile), "done")
		},
		verify: func(t *testing.T, stageDir, binDir string, restored int, err error) {
			if err != nil {
				t.Fatalf("converge: %v", err)
			}
			got, _ := os.ReadFile(filepath.Join(binDir, WorkerBinaryName))
			if string(got) != "V1-old" {
				t.Errorf("worker.exe = %q, want %q", got, "V1-old")
			}
			if _, statErr := os.Stat(filepath.Join(binDir, SupervisorBinaryName+".new")); !os.IsNotExist(statErr) {
				t.Errorf("stale supervisor .new must be cleaned")
			}
			if IsSupervisorUpgradePending(stageDir) {
				t.Errorf("pending sentinel must be cleaned")
			}
			// 旧 rollback.done 必须清除 — 残留会让 superviseWorker 对本轮
			// 新版本误卸载健康监视（upgrade watch 死锁的另一形态）
			if RollbackDoneExists(stageDir) {
				t.Errorf("stale rollback.done must be cleaned")
			}
		},
	},
}

// TestConvergeResidualBackups_PowerLossStates 断电组合态表驱动。
func TestConvergeResidualBackups_PowerLossStates(t *testing.T) {
	for _, tc := range convergeCases {
		t.Run(tc.name, func(t *testing.T) {
			stageDir := t.TempDir()
			binDir := t.TempDir()
			tc.preset(t, stageDir, binDir)

			// 入口收敛：entries 覆盖预置 dest（dest 目录去重后传入）
			entries := []ManifestEntry{
				{Src: WorkerBinaryName, Dest: filepath.Join(binDir, WorkerBinaryName)},
				{Src: SupervisorBinaryName, Dest: filepath.Join(binDir, SupervisorBinaryName)},
			}
			restored, err := ConvergeResidualBackups(stageDir, binDir, entries)
			tc.verify(t, stageDir, binDir, restored, err)
		})
	}
}

// TestConvergeResidualBackups_CleanState_NoOp 验证干净状态下收敛是零变动 no-op。
func TestConvergeResidualBackups_CleanState_NoOp(t *testing.T) {
	stageDir := t.TempDir()
	binDir := t.TempDir()
	writeTestFile(t, filepath.Join(binDir, WorkerBinaryName), "V1")

	restored, err := ConvergeResidualBackups(stageDir, binDir, []ManifestEntry{
		{Src: WorkerBinaryName, Dest: filepath.Join(binDir, WorkerBinaryName)},
	})
	if err != nil {
		t.Fatalf("converge on clean state: %v", err)
	}
	if restored != 0 {
		t.Errorf("restored = %d, want 0 on clean state", restored)
	}
	got, _ := os.ReadFile(filepath.Join(binDir, WorkerBinaryName))
	if string(got) != "V1" {
		t.Errorf("worker.exe = %q, must be untouched", got)
	}
}

// TestMachineApply_ConvergesResidualThenApplies 验证半换断电死锁态经 Machine.Apply
// 自动收敛：备份残留 + 健康标记匹配旧值 → 重新 apply 后自动回到可用路径。
func TestMachineApply_ConvergesResidualThenApplies(t *testing.T) {
	stageDir := t.TempDir()
	binDir := t.TempDir()

	// 预置死锁态：断电在 swap 后、WriteUpgradeMeta 前
	writeTestFile(t, filepath.Join(binDir, WorkerBinaryName), "V2-broken")
	writeTestFile(t, filepath.Join(binDir, WorkerBinaryName+PreviousSuffix), "V1-old")
	writeTestFile(t, filepath.Join(stageDir, LastUpgradeVerFile), "V1")
	writeTestFile(t, filepath.Join(stageDir, HealthyMarkerFile), "V1")

	// 新 bundle v3
	buildMachineBundle(t, stageDir, binDir, "v3", []struct{ Src, Dest, Content string }{
		{WorkerBinaryName, WorkerBinaryName, "V3-worker"},
	})

	m := NewMachine(stageDir, binDir, testLogger(), nil)
	if err := m.Apply(context.Background(), 0); err != nil {
		t.Fatalf("Machine.Apply on residual state: %v", err)
	}

	// 新版本落地
	got, _ := os.ReadFile(filepath.Join(binDir, WorkerBinaryName))
	if string(got) != "V3-worker" {
		t.Errorf("worker.exe = %q, want %q", got, "V3-worker")
	}
	// 关键断言：.previous 内容 = V1-old（收敛先行）。若无收敛，apply 会把
	// V2-broken 备份成 .previous — 健康超时回滚将恢复到坏版本
	got, _ = os.ReadFile(filepath.Join(binDir, WorkerBinaryName+PreviousSuffix))
	if string(got) != "V1-old" {
		t.Errorf(".previous = %q, want %q (convergence must run before swap)", got, "V1-old")
	}
	// 版本元数据已更新为新 bundle
	if v := readTrimmed(LastUpgradeVerPath(stageDir)); v != "v3" {
		t.Errorf("last_upgrade_ver = %q, want v3", v)
	}
}
