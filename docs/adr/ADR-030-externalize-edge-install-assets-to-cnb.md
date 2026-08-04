# ADR-030：将 Edge 安装制品拆分为 CNB Release 直链附件

> 完整规范见 `spec/02-architecture/architecture-decision-record.md`（何时写、状态流转、评审流程）。

- 状态：已接受
- 日期：2026-08-04
- 作者：Codex
- 替代：不适用

## 背景

Compose 安装包长期同时携带 Manager 配置、`ongrid-edge` 和八个第三方采集组件。当前 amd64 Edge 裸制品中，第三方组件约 508.6 MiB，自研 `ongrid-edge` 约 13.9 MiB。第三方版本很少变化，却会在每个 Ongrid 版本和每种 Manager 安装包中重复出现，增加安装包体积、上传时间和制品存储。

Manager 当前通过本地 `/opt/ongrid/edge` 同时提供 Edge 网络安装器需要的单文件下载路径和 ADR-024 一键升级 bundle。直接把 Edge 设备的下载地址改成外部仓库会改变既有部署拓扑和升级协议，因此只改变 Manager 安装时的制品来源，设备侧继续访问原有 `/edge/` 接口。

## 决策

### 1. 公共依赖上传一次

八个第三方组件按架构分别归档为：

- `edge-deps-linux-amd64.tar.xz`
- `edge-deps-linux-arm64.tar.xz`

它们上传到一个由布局修订号和全部上游版本组成的不可变 CNB Release tag，例如：

`edge-deps-layout1-p3.4.0-o0.118.0-n1.8.2-pr0.8.4-my0.19.0-pg0.19.1-r1.86.0-m0.51.0`

每个归档附带外部 `.sha256`，内部还包含 `TARGET`、`DEPENDENCIES` 和 `MANIFEST.sha256`。发布命令发现该 Release 已存在全部预期附件时直接复用；只存在部分附件时失败，禁止悄悄覆盖不可变内容。只有依赖版本集合或布局发生变化才创建新的公共依赖 Release。

公共依赖归档使用固定顺序、时间戳、owner/group 和压缩块大小生成，确保相同输入重复构建得到相同 SHA-256。发布命令会先检查完整的既有 Release，在下载上游依赖或执行 Go 构建前直接跳过，因此能够继续复用修复前已经发布的不可变附件。附件上传器必须固定到经过审核的镜像 digest，禁止把 CNB 发布令牌交给可变 tag。

### 2. 自研 Edge 跟随 Ongrid 版本

每个 Ongrid Release 只新增：

- `ongrid-edge-linux-amd64-<version>` 及 `.sha256`
- `ongrid-edge-linux-arm64-<version>` 及 `.sha256`

例如 `v0.11.1` 的下载地址为：

`https://cnb.cool/ongridio/ongrid-edge/-/releases/download/v0.11.1/ongrid-edge-linux-amd64-v0.11.1`

安装端使用普通 HTTPS `curl` 直接下载，不需要 Docker 拉取制品镜像、创建临时容器或从镜像层复制文件。

GitHub 的 `Release` workflow 在每个 `vMAJOR.MINOR.PATCH` tag 上自动执行
`edge-release` job：校验公共依赖 Release、在轻量 `ongridio/ongrid-edge`
仓库中幂等创建同版本 CNB Release、构建两个 Linux 架构的自研二进制并上传。
最终 GitHub Release 必须等待该 job 成功，避免主 Release 已发布而 Edge 下载缺失。

### 3. Manager 安装时预取并重建兼容目录

标准安装包只携带 Edge 安装脚本、systemd unit、附件下载脚本、依赖 Release 锁文件和 bundle 构建脚本，不再默认携带大型二进制。

`install.sh` 和 `upgrade.sh` 执行以下步骤：

1. 从公共依赖 Release 下载目标架构归档，从当前 Ongrid Release 下载 `ongrid-edge`；
2. 将 sidecar 中的 SHA-256 和文件名严格绑定到当前附件，并校验依赖归档内部 manifest 和目标架构；
3. 把校验通过的附件保存在 `/var/cache/ongrid/edge-artifacts`，重复安装或升级直接复用；
4. 在安装目录同一文件系统的隐藏 staging 目录中生成原有单文件布局和 ADR-024 bundle；
5. 全部成功后再原子替换 `/opt/ongrid/edge`。

升级预取发生在停止旧 Compose 栈之前。网络不可达、附件缺失、checksum 不符或架构不匹配时，旧服务和旧 `/edge` 目录保持不变。

完成 `/edge` 原子替换后、健康检查成功前发生错误时，错误处理会恢复交换前的 Edge 目录；健康检查仅超时时保留并输出备份路径，供运维人员手工判断和回滚。

Manager、Nginx 和 Edge 设备继续使用既有 `/edge/` URL、文件名、manifest 与 apply 流程，不感知外部附件来源。

### 4. 保留离线兼容路径

隔离环境可在构建安装包时设置 `ONGRID_BUNDLE_EDGE_ASSETS=1`，恢复原有内嵌二进制布局。安装器检测到内嵌 `ongrid-edge-linux-*` 后跳过 CNB 下载，其余 bundle 和目录替换路径不变。

默认只服务 `linux-amd64` Edge；需要同时服务两种架构时设置 `ONGRID_EDGE_TARGETS="linux-amd64 linux-arm64"`。私有代理或镜像站可通过 `ONGRID_EDGE_ARTIFACT_BASE_URL` 覆盖下载根地址。

## 后果

### 正面影响

- Manager 安装包不再重复携带约 500 MiB 裸 Edge 公共组件；
- 公共组件跨 Ongrid 版本只存储和上传一次；
- 常规发版只新增两个较小的自研 `ongrid-edge` 二进制及 checksum；
- 目标主机无需为了获取二进制而拉 OCI 镜像；
- 下载或校验失败发生在升级停机前；
- Edge 设备安装、在线升级和回滚协议保持兼容。

### 负面影响与权衡

- 标准在线安装新增对 CNB Release 附件直链的依赖；
- GitHub Actions 需要配置 `CNB_TOKEN`；自动创建 Release 需要
  `repo-release:rw`，附件插件需要 `repo-contents:rw`，同时还需保留现有镜像和 Helm 发布权限；
- Manager 主机仍需保留解压后的单文件和兼容 bundle，节省的是发布包、传输和远端重复制品，不是运行目录全部空间；
- 服务多个 Edge 架构会增加 Manager 本地缓存和提取空间；
- 公共组件升级必须创建新的不可变依赖 tag，不能覆盖旧附件。

## 验证要求

- 附件构建必须验证两个 Linux 架构的必需组件全集并生成内外两层 checksum；
- 下载脚本测试必须覆盖直链路径、缓存复用、完整提取和 checksum 篡改拒绝；
- 发布脚本必须完整存在时幂等跳过、部分存在时拒绝覆盖；
- Release 创建脚本必须在目标已存在时幂等复用，API 权限不足时失败且不得输出 Token；
- GitHub Release workflow 必须等待 CNB `edge-release` job 成功；
- 发布包测试必须证明默认包包含依赖 tag 锁文件和下载脚本，但不包含 Edge 大型二进制；
- 升级脚本必须保持“附件预取成功后才停止旧服务”的顺序。
