// converge.go 实现 apply 入口残留收敛。
//
// 语义依据：上次 apply 断电/失败留下的 .previous 残留与
// 「合法备份」同形，且断电点若在 WriteUpgradeMeta 前，last_upgrade_ver 仍是
// 旧值且匹配 healthy_marker（假阳性健康态）→ 回滚永不触发、CleanupPrevious
// 不可达 → 拒绝语义 = 无出口死锁。故入口不拒绝，改为先幂等收敛（恢复旧版 +
// 清理暂存物与陈旧哨兵）再继续本轮 apply。

package upgrademachine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ConvergeResidualBackups 在 apply 入口收敛上次崩溃轮次的残留物：
//
//  1. Rollback 恢复 binDir 与各 entry dest 目录下的全部 *.previous（幂等：
//     干净状态 = 0 个 no-op）；恢复集 = 遍历到的全部备份，含「备份后改名前」
//     断电的未换条目
//  2. 清理 supervisor 暂存 .new（entry dest 侧 + binDir 侧）与
//     supervisor_upgrade.pending 哨兵 — 本轮 manifest 若含 supervisor 条目，
//     applyOne 会重新 stage，清旧不碍新
//  3. 清理 stale rollback.done — 残留会让 superviseWorker 对本轮新版本误卸载
//     健康监视（正常流程由 edge DownloadBundle 步骤 2 删除，issue ；此处
//     是 apply 侧的幂等兜底）
//
// 必须在 ValidateAllEntries 之后调用：dirs 集合源自 manifest 条目，未校验的
// manifest 不得驱动任意目录的文件操作。必须在 kill 之后调用：恢复 rename
// 可能撞运行中进程的 image 锁。
//
// 返回恢复的文件数。恢复失败返回 error（与 Rollback 同语义：整体失败可重试），
// 清理失败聚合进同一 error。
func ConvergeResidualBackups(stageDir, binDir string, entries []ManifestEntry) (int, error) {
	dirs := dirSet(binDir, entries)

	restored, err := Rollback(dirs)
	if err != nil {
		return restored, fmt.Errorf("converge residual backups: %w", err)
	}

	var errs []error
	// supervisor 暂存 .new：entry dest 侧（applyOne 写）+ binDir 侧
	// （SupervisorSelfSwap 重定位源，覆盖非 BinDir 安装路径场景）
	newPaths := []string{filepath.Join(binDir, SupervisorBinaryName+".new")}
	for _, e := range entries {
		if isSupervisorBinary(e.Dest) {
			newPaths = append(newPaths, e.Dest+".new")
		}
	}
	if err := cleanupSupervisorStaging(stageDir, newPaths...); err != nil {
		errs = append(errs, err)
	}
	if err := os.Remove(RollbackDonePath(stageDir)); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("remove stale rollback.done: %w", err))
	}
	return restored, errors.Join(errs...)
}

// cleanupSupervisorStaging 删除 supervisor 暂存 .new 集合 + pending 哨兵。
// （apply 失败自恢复）与 D2（入口残留收敛）共用的清理原语。
// 幂等：路径不存在视为成功。
func cleanupSupervisorStaging(stageDir string, newPaths ...string) error {
	var errs []error
	for _, p := range newPaths {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove staged %s: %w", p, err))
		}
	}
	if err := os.Remove(SupervisorUpgradePendingPath(stageDir)); err != nil && !os.IsNotExist(err) {
		errs = append(errs, fmt.Errorf("remove supervisor pending sentinel: %w", err))
	}
	return errors.Join(errs...)
}

// dirSet 返回 binDir + 各 entry dest 目录的去重集合（Rollback 的遍历范围）。
func dirSet(binDir string, entries []ManifestEntry) []string {
	seen := map[string]bool{binDir: true}
	dirs := []string{binDir}
	for _, e := range entries {
		d := filepath.Dir(e.Dest)
		if !seen[d] {
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	return dirs
}
