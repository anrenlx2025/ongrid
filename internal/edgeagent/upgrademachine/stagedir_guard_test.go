// stagedir_guard_test.go 测试 stage 目录安全门控（stageDirGuard）：
// guard 校验失败时 CheckPending / BootCheck 拒绝消费 pending bundle（fail-closed），
// 且不影响 rollback / self-swap 恢复路径。

package upgrademachine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeBarePending 写入裸 pending 文件（不解压内容，仅让 HasPendingBundle 为 true）。
func writeBarePending(t *testing.T, stageDir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(stageDir, PendingFileName), []byte("fake-tar-gz"), 0o600); err != nil {
		t.Fatalf("write pending: %v", err)
	}
}

// TestCheckPending_StageDirGuardRefuses 验证 guard 失败时 CheckPending 返回 nil
// （视同无 pending），pending 不被解压、不被消费。
func TestCheckPending_StageDirGuardRefuses(t *testing.T) {
	stageDir := t.TempDir()
	binDir := t.TempDir()
	writeBarePending(t, stageDir)

	m := NewMachine(stageDir, binDir, testLogger(), nil)
	m.SetStageDirGuard(func() error { return errors.New("acl: dir is writable by Users") })

	err := m.CheckPending(context.Background(), 0)
	if err != nil {
		t.Fatalf("CheckPending 应返回 nil（升级通道关闭但不报错）： %v", err)
	}

	// pending 应原样保留（未被消费）
	if !HasPendingBundle(stageDir) {
		t.Error("pending bundle 不应被消费")
	}
	// incoming/ 不应被创建（未解压）
	if _, err := os.Stat(filepath.Join(stageDir, "incoming")); !os.IsNotExist(err) {
		t.Errorf("incoming/ 不应被创建（err=%v）", err)
	}
}

// TestBootCheck_StageDirGuardSkipsPendingApply 验证 guard 失败时 BootCheck
// 跳过 pending 解压/apply 并把 guard 错误记入 lastErr（向上可见）。
func TestBootCheck_StageDirGuardSkipsPendingApply(t *testing.T) {
	stageDir := t.TempDir()
	binDir := t.TempDir()
	writeBarePending(t, stageDir)

	m := NewMachine(stageDir, binDir, testLogger(), nil)
	m.SetStageDirGuard(func() error { return errors.New("acl: dir is writable by Users") })

	err := m.BootCheck(context.Background())
	if err == nil {
		t.Fatal("BootCheck 应把 guard 错误记入 lastErr 向上返回")
	}

	// pending 应原样保留（未解压、未 apply）
	if !HasPendingBundle(stageDir) {
		t.Error("pending bundle 不应被消费")
	}
	if _, statErr := os.Stat(filepath.Join(stageDir, "incoming")); !os.IsNotExist(statErr) {
		t.Errorf("incoming/ 不应被创建（err=%v）", statErr)
	}
}

// TestCheckPending_NilGuardAllowsConsumption 验证 guard 为 nil（无本地多用户
// 威胁的平台 / 测试）时行为与门控引入前一致：pending 走正常解压路径。
func TestCheckPending_NilGuardAllowsConsumption(t *testing.T) {
	stageDir := t.TempDir()
	binDir := t.TempDir()
	writeBarePending(t, stageDir)

	m := NewMachine(stageDir, binDir, testLogger(), nil)
	if err := m.checkStageDir(); err != nil {
		t.Fatalf("nil guard 应恒通过： %v", err)
	}
}

// TestEqualFoldPrefix 验证 Windows 大小写不敏感前缀判定的边界。
func TestEqualFoldPrefix(t *testing.T) {
	cases := []struct {
		s, prefix string
		want      bool
	}{
		{`C:\a\dst\file`, `C:\A\DST\`, true},   // 大小写不同
		{`C:\A\DST\file`, `c:\a\dst\`, true},   // 全小写 prefix
		{`C:\a\dstx\file`, `C:\a\dst\`, false}, // 前缀是目录名的部分匹配，必须拒绝
		{`C:\a\ds`, `C:\a\dst\`, false},        // s 比 prefix 短
		{`C:\a\dst`, `C:\a\dst\`, false},       // 完全相等但无尾分隔符场景由调用方处理
		{"", "", true},                         // 空对空
		{"abc", "", true},                      // 空 prefix 恒匹配
		{"", "a", false},
	}
	for _, c := range cases {
		if got := EqualFoldPrefix(c.s, c.prefix); got != c.want {
			t.Errorf("EqualFoldPrefix(%q, %q) = %v, want %v", c.s, c.prefix, got, c.want)
		}
	}
}
