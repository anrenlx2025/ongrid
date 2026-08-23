//go:build windows

package install

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ACL 验证：DPAPI CRYPTPROTECT_LOCAL_MACHINE scope 绑定到机器级 SystemCredential，
// 同机器上任何 LocalSystem / NetworkService 进程都能解密。这意味着 DPAPI 本身
// 不替代文件 ACL——必须配合文件系统 ACL 限制非 System/Administrators 身份的读访问。
//
// 目标 ACL（secrets.enc + 受管目录）：
//   - NT AUTHORITY\SYSTEM (S-1-5-18)        — supervisor / worker 跑在 LocalSystem
//   - BUILTIN\Administrators (S-1-5-32-544) — 管理员运维访问
//   - 不允许任何其他 trustee 的 ACE
//
// 实现策略：
//   - Apply 用 icacls.exe（内置命令，可审计），grant 目标用 SID 而非账户名
//     （`*S-1-5-18`），免疫非英文系统上账户名的本地化差异
//   - Verify 用 GetNamedSecurityInfo 原生 API 逐 ACE 读出 trustee SID，做
//     正向白名单校验：每个 ACE 的 SID 必须 ∈ {SYSTEM, Administrators}。
//     黑名单式检查（只查 Users/Everyone 等通配组）检不出预植的具体用户 ACE
//
// 路径安全：所有系统工具（icacls）用 %SystemRoot%\System32 绝对路径调用。
// SYSTEM 服务继承系统 PATH，第三方软件常向其中注入用户可写目录 — 相对名
// 解析会让 icacls 以 LocalSystem 执行被劫持的副本。

// 允许的 trustee SID（本地化无关的稳定标识）。
const (
	sidSYSTEM = "S-1-5-18"     // NT AUTHORITY\SYSTEM
	sidAdmins = "S-1-5-32-544" // BUILTIN\Administrators

	// fileAllAccess = FILE_ALL_ACCESS（icacls 显示为 (F)）。
	fileAllAccess = 0x001F01FF
)

// archiveDirFn 是 EnsureSecureDir 归档预植目录的测试缝（生产 = os.Rename）。
var archiveDirFn = os.Rename

// systemTool 返回系统工具的绝对路径（%SystemRoot%\System32\<name>）。
// 绝对路径解析是硬要求：见文件头注释的 PATH 劫持说明。
func systemTool(name string) (string, error) {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = os.Getenv("windir")
	}
	if root == "" {
		return "", fmt.Errorf("SystemRoot environment variable not set; cannot resolve %s", name)
	}
	p := filepath.Join(root, "System32", name)
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("system tool %s not found at %s: %w", name, p, err)
	}
	return p, nil
}

// sanitizeIcaclsOutput 从 icacls 输出中提取关键错误信息，去除路径前缀和 SID 详情。
// 用于 error message，避免泄露完整路径/ACL 配置到生产日志。
func sanitizeIcaclsOutput(out []byte) string {
	s := strings.TrimSpace(string(out))
	// 取最后一行（通常是 "Successfully processed N files" 或错误摘要）
	lines := strings.Split(s, "\n")
	if len(lines) == 0 {
		return ""
	}
	last := strings.TrimSpace(lines[len(lines)-1])
	// 截断超长输出
	if len(last) > 200 {
		last = last[:200] + "..."
	}
	return last
}

// icaclsPath 返回 icacls.exe 的绝对路径（Server Core 精简镜像可能缺失）。
func icaclsPath() (string, error) {
	return systemTool("icacls.exe")
}

// runIcacls 执行 icacls 并返回 CombinedOutput / error。
func runIcacls(args ...string) ([]byte, error) {
	exe, err := icaclsPath()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("icacls %s failed (exit %v): %s",
			strings.Join(redactPaths(args), " "), err, sanitizeIcaclsOutput(out))
	}
	return out, nil
}

// redactPaths 把参数列表中的绝对路径替换为 <path>（错误信息防泄露）。
func redactPaths(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if filepath.IsAbs(a) {
			out[i] = "<path>"
		} else {
			out[i] = a
		}
	}
	return out
}

// ApplySecretACL 应用 secrets.enc 的 ACL：仅 SYSTEM + Administrators 有 Full Control。
//
// 操作：
//  1. /inheritance:r  — 移除从父目录继承的 ACE
//  2. /grant:r        — 替换为指定 ACE（非合并）
//
// grant 目标用 SID（`*S-1-5-18`）而非账户名：icacls 的 /grant 接受 SID，
// 且不受系统语言/locale 影响账户名翻译。
//
// 注意：(F) Full Control 是必要的——supervisor（SYSTEM 身份）需要 Write + Delete
// 来执行 Rotate（rename tmp → secrets.enc 会用 Delete 旧文件）。
func ApplySecretACL(path string) error {
	_, err := runIcacls(filepath.Clean(path),
		"/inheritance:r",
		"/grant:r",
		"*"+sidSYSTEM+":(F)",
		"*"+sidAdmins+":(F)",
	)
	return err
}

// ApplyDirACL 应用目录的 ACL，使用 (OI)(CI) 容器继承标记让子文件/子目录自动继承。
//
// 与 ApplySecretACL 区别：
//   - 目录用 (OI)(CI)(F) — Object Inherit + Container Inherit + Full Control
//   - secrets.enc 用 (F) — 仅文件本身
//
// 局限：/inheritance:r 只移除继承 ACE，不清除已存在的显式 ACE。预植目录
// （攻击者在 install 前创建并给自己授予显式 ACE）由 EnsureSecureDir 的
// 归档重建路径处理，不依赖本函数。
//
// 调用 EnsureSecureDir 而非直接调用此函数（EnsureSecureDir 含 MkdirAll + 验证）。
func ApplyDirACL(dir string) error {
	_, err := runIcacls(filepath.Clean(dir),
		"/inheritance:r",
		"/grant:r",
		"*"+sidSYSTEM+":(OI)(CI)(F)",
		"*"+sidAdmins+":(OI)(CI)(F)",
	)
	return err
}

// verifyACLStrict 验证 path 的 DACL 符合正向白名单：
//   - 必须存在 SYSTEM 与 Administrators 的 allow-full ACE
//   - 不包含任何其他 trustee 的 ACE（具体用户、预植显式 ACE、deny ACE 全部 fail）
//
// 用 GetNamedSecurityInfo 原生 API（不经 icacls 文本输出）：
//   - SID 级比较，免疫非英文系统上 icacls 输出的账户名本地化
//   - 黑名单式检查检不出"陌生 SID"，正向白名单才闭合预植面
func verifyACLStrict(path string) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read DACL of %s: %w", filepath.Base(path), err)
	}
	dacl, present, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("get DACL of %s: %w", filepath.Base(path), err)
	}
	if !present || dacl == nil {
		return fmt.Errorf("no DACL present on %s (unrestricted access)", filepath.Base(path))
	}

	seenSYSTEM, seenAdmins := false, false
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return fmt.Errorf("read ACE %d of %s: %w", i, filepath.Base(path), err)
		}
		// SID 紧跟 ACE 头部（SidStart 字段即 SID 首字节；deny ACE 布局相同）。
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		full := ace.Mask&fileAllAccess == fileAllAccess
		switch sid.String() {
		case sidSYSTEM:
			if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !full {
				return fmt.Errorf("SYSTEM ACE on %s is not allow-full", filepath.Base(path))
			}
			seenSYSTEM = true
		case sidAdmins:
			if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !full {
				return fmt.Errorf("Administrators ACE on %s is not allow-full", filepath.Base(path))
			}
			seenAdmins = true
		default:
			return fmt.Errorf("unexpected ACE trustee on %s (allowlist: SYSTEM, Administrators only)",
				filepath.Base(path))
		}
	}
	if !seenSYSTEM || !seenAdmins {
		return fmt.Errorf("missing required ACE on %s (need SYSTEM + Administrators allow-full)", filepath.Base(path))
	}
	return nil
}

// VerifySecretACL 验证 secrets.enc 的 ACL 符合预期（正向白名单，见 verifyACLStrict）。
//
// 这是"独立检查点"——即使 ApplySecretACL 成功，也要读回来验证。
// 防止 GPO / AV / 其他工具在 apply 后修改 ACL。
func VerifySecretACL(path string) error {
	return verifyACLStrict(filepath.Clean(path))
}

// VerifyDirACL 验证目录 ACL 符合预期（正向白名单，见 verifyACLStrict）。
func VerifyDirACL(dir string) error {
	return verifyACLStrict(filepath.Clean(dir))
}

// VerifyTreeACL 验证 root 目录及其现有子树内每个条目的 DACL 都是严格白名单。
//
// 运行期门控用（stage 目录消费前）：顶层目录 ACL 正确不代表子项正确——
// 子项（pending / incoming / 哨兵文件）的 DACL 可被持 WRITE_DAC 的主体事后
// 放宽，消费前逐项复验才闭合。只校验已存在的条目。
func VerifyTreeACL(root string) error {
	clean := filepath.Clean(root)
	if err := verifyACLStrict(clean); err != nil {
		return fmt.Errorf("root: %w", err)
	}
	return filepath.WalkDir(clean, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == clean {
			return nil
		}
		return verifyACLStrict(p)
	})
}

// EnsureSecureDir 确保 dir 存在且 ACL 受限（仅 SYSTEM + Administrators）。
//
// 流程：
//  1. dir 已存在时先严格验证：通过 = 保留（幂等重装不动数据）；
//     不通过 = 视为不可信（可能是 install 前预植的目录——/inheritance:r
//     清不掉显式 ACE），归档改名后全新重建。全新创建的目录不含任何
//     显式 ACE，只有我们授予的两条。
//  2. MkdirAll(dir) — 创建目录（如已存在是 noop）
//  3. ApplyDirACL(dir) — 刷新 ACL（幂等）
//  4. VerifyDirACL(dir) — 读回来验证（fail-closed）
//
// 归档产物 <dir>.preexisting-<unix-ts> 保留在磁盘上（不删除数据），
// 其 ACL 不受控但路径已脱离信任根。
//
// 调用时机：Install 流程开始前（写 secrets.enc 之前）确保父目录受限，
// 这样 secrets.enc 即使继承父目录 ACL 也只对 SYSTEM+Admins 可读。
func EnsureSecureDir(dir string) error {
	clean := filepath.Clean(dir)
	if fi, err := os.Stat(clean); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("%s exists and is not a directory", clean)
		}
		if err := verifyACLStrict(clean); err != nil {
			arch := fmt.Sprintf("%s.preexisting-%d", clean, time.Now().Unix())
			if err := archiveDirFn(clean, arch); err != nil {
				return fmt.Errorf("dir %s has untrusted ACL (refusing to reuse; likely pre-planted): archive to %s failed: %w", clean, arch, err)
			}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", clean, err)
	}
	if err := os.MkdirAll(clean, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", clean, err)
	}
	if err := ApplyDirACL(clean); err != nil {
		return fmt.Errorf("apply dir ACL: %w", err)
	}
	if err := VerifyDirACL(clean); err != nil {
		return fmt.Errorf("verify dir ACL: %w", err)
	}
	return nil
}
