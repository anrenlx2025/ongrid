# PRD-004：外部 Elasticsearch 日志中心

## 元信息

- 状态：开发中
- 作者：Codex
- 日期：2026-08-18
- 关联 Epic：不适用
- 关联 RFC：[RFC-003：OTel 日志采集与 Elasticsearch 直写](../rfc/RFC-003-otel-elasticsearch-logs.md)
- 关联 ADR：[ADR-031：Edge 直写外部 Elasticsearch](../adr/ADR-031-edge-direct-elasticsearch-logs.md)
- 关联 HLD：[HLD-001：日志采集、存储与查询解耦](../design/HLD-001-log-pipeline-backend-abstraction.md)

## 背景

现有日志中心以 Loki/LogQL 为唯一存储和查询模型，Edge 的 `logs` 插件由 Promtail 实现。它适合基于标签缩小日志流，但产品缺少面向日志正文的稳定全文检索、短语搜索、字段聚合、上下文和深分页能力。仅让 Edge 把数据写进 Elasticsearch 不能形成可用产品，因为当前页面、告警、AIOps、Incident 关联和 Grafana 跳转都直接绑定 Loki/LogQL。

## 目标

- 新增外部 Elasticsearch 8.16+ 日志后端，日志正文由 Edge 直接写入，Manager 不承载写入字节流。
- 用 `otelcol-contrib` 替换 Promtail，同时保留 `logs` 插件名和现有控制通道。
- 提供后端无关的日志查询 API，支持 Loki 与 Elasticsearch，并让页面不再拼接后端 DSL。
- 支持关键字、短语、包含/排除、字段筛选、时间直方图、上下文和游标分页。
- 保留内置 Loki 作为默认与回滚后端；Elasticsearch 故障时无需退回 Promtail。
- 正常状态端到端日志可见延迟 P95 不超过 10 秒；15 分钟单集群查询 P95 不超过 3 秒。

## 功能需求

### 外部 Elasticsearch 接入

- 管理员可以保存写入 endpoint 列表、Manager 查询 endpoint、数据流 namespace、自定义 CA、写 API Key 和只读 API Key。
- 写凭证与查询凭证必须分离；敏感值不可由读取 API 返回。
- 激活前必须分别通过 Manager 查询探针和选定 Edge 的真实写入探针。
- 后端状态包含草稿、分发中、验证中、已激活、降级和已回滚。
- 每个 Edge 展示期望版本、已应用版本、最后写入成功时间和错误原因。

### 日志采集

- Host 支持 journald 和多个独立文件源。
- Kubernetes Node 支持 CRI 容器日志并补全 cluster、namespace、pod、container、node 和 workload 字段。
- 每个文件源可以配置稳定 source id、service name、dataset、include/exclude、JSON/regex/plain parser 和 multiline。
- Collector 重启后必须从持久化 file offset/journal cursor 继续读取。
- 下游暂时失败时使用有界持久化发送队列，不允许无限占满系统盘或静默丢弃。

### 日志查询

- 页面通过结构化查询请求搜索，不直接提交 Elasticsearch DSL 或新建 LogQL。
- 支持设备、角色、集群、namespace、workload、pod、container、service、source、severity 和文件筛选。
- 支持全文关键字、短语、包含任一/全部、排除关键字。
- 支持时间直方图、字段名/字段值、上下文日志、游标分页、自动刷新和受限导出。
- 灰度期间选定 Edge 有界双写当前权威后端和候选 ES，查询保持读取权威后端且无日志盲区；全局切换后按 `cutover_at/ended_at` 读取 Loki 和各历史 ES generation。

### 兼容功能

- 新增后端无关 `search_logs` AIOps 工具；旧 `query_logql` 仅在 Loki 可用时保留。
- 新增结构化日志匹配告警；原始 LogQL 规则标记为 Loki-only，存在未迁移启用规则时阻止 ES 全量激活。
- Incident 日志关联消费统一日志结果，不再解析 Loki stream。
- ES 模式默认跳转产品日志页；只有配置 Kibana URL 时显示 Discover 跳转。

## 边界情况

- Edge 能访问写 endpoint 但 Manager 不能访问查询 endpoint时，不允许激活。
- Manager 查询成功但 Edge 无法完成真实写探针时，不允许激活。
- 离线 Edge 可能继续采集并写旧 generation，因此全量激活和回滚必须阻塞，直到所有启用 logs 的 Edge 在线并完成真实写探针；不得把离线 Edge 静默排除在全局时间线之外。
- 灰度期间候选后端允许短时间重复存储，但用户查询只返回权威后端；不接受无提示的日志缺口，交付语义为 at-least-once。
- 队列达到高水位后暂停读取并告警；源文件或 journal 自身被轮转淘汰导致的损失必须可观测。
- 用户配置的任意字段不能直接成为 data stream 名或动态 mapping 根字段。
- 不允许通过查询 API 访问 Ongrid 日志数据流之外的索引。

## 非功能需求

- **兼容性**：第一期支持 Elasticsearch 8.16+；不声明 OpenSearch、ES 7、Cloud ID 或原始 DSL 支持。
- **安全**：HTTPS 默认开启；凭证加密存储、最小权限、可轮换且不进入普通插件快照、参数和日志。
- **可靠性**：配置原子写入并在重启前校验；失败保持上一份工作配置。
- **性能**：默认查询窗口 1 小时、结果上限 1000；后端使用 PIT + `search_after`，禁止无界深分页。
- **可观测性**：暴露 receiver、exporter、队列、存储、generation、最后成功时间和错误类别。
- **运维**：先灰度后全量；不做永久生产双写；内置 Loki 为稳定回滚出口。

## API 变更

- 新增日志后端管理、连接测试、Edge 写探针、激活、回滚和 rollout 状态 API。
- 新增 `/api/v1/logs/search`、`/histogram`、`/fields`、`/context`。
- 保留现有 `/api/v1/logs/query_range` 和 label API 作为兼容接口。
- 新增 tunnel `write_plugin_secrets` RPC；普通插件快照只增加 backend generation 和非敏感目标信息。

## 数据库变更

- 新增 `log_backends`：保存非敏感配置、凭证引用、状态和 generation。
- 新增 `log_backend_assignments`：保存 Edge 期望/应用 generation、cutover 和探针状态。
- 复用 `secrets` 表保存加密的 Elasticsearch 写、读凭证。
- 所有 schema 变更使用 expand-contract，旧 Manager/Edge 在滚动升级期仍可读取原配置。

## 依赖与阻塞

- 依赖现有凭证库、Edge 插件控制通道和 `otelcol-contrib` 制品。
- 依赖用户准备匹配 `logs-ongrid.*.otel-*` 的 data stream template/生命周期策略。
- journald receiver 为上游 alpha 组件，必须通过产品 Linux 矩阵验证后才能默认启用。
- 无产品决策阻塞项。

## 风险与假设

- Elasticsearch exporter 和 filelog 为 beta、journald 为 alpha；通过固定版本、配置校验、故障注入和上一 Ongrid 发布版本的紧急回退缓解。
- 旧 Promtail positions 不能无损转换为 OTel checkpoint；使用预热 checkpoint 和短重复窗口避免缺口。
- 第一阶段共享 fleet 写 API Key 的泄露影响面较大；写权限必须限定到产品 data stream，后续支持按集群/Edge 分配。
- 外部 ES 容量、ILM、备份由客户负责；产品只做激活前检查和状态展示。

## 验收标准

- [ ] Host journald、文件和 Kubernetes CRI 日志均可由 OTel 收集并写入 ES。
- [ ] 抓包证明外部 ES 日志正文不经过 Manager。
- [ ] 日志页面在 ES 下支持关键字、短语、字段筛选、直方图、上下文和游标分页。
- [ ] Collector/Edge 重启后 offset/cursor 可恢复，文件轮转测试无从头回放。
- [ ] 断网 30 分钟后队列续传，达到边界时有明确告警且不占满系统盘。
- [ ] 写、读 API Key 不出现在数据库明文、普通快照、进程参数、环境变量、日志或 API 响应。
- [ ] ES 激活前真实 Edge 写探针与 Manager 读探针均成功。
- [ ] 迁移期间跨切换时间查询能归并 Loki 与 ES 数据。
- [ ] 未迁移的启用 LogQL 告警会阻止 ES 全量激活。
- [ ] ES 故障可在 5 分钟内将同一个 OTel 流水线切回内置 Loki。
- [ ] AMD64/ARM64 配置校验、Go race 测试、前端测试、构建和深浅主题截图通过。

## 优先级

P0

## 排期

- 开始：2026-08-18
- 目标完成：2026-09-18

## 任务拆分（PRD → Tasks）

- [ ] Task 1：规格、版本支持矩阵和配置 golden tests。
- [ ] Task 2：统一日志领域模型、查询 API 和 Loki adapter。
- [ ] Task 3：Elasticsearch query adapter、data stream 约束和查询保护。
- [ ] Task 4：后端配置、数据库、加密凭证和激活状态机。
- [ ] Task 5：通用插件密钥投递、Edge 原子应用和状态上报。
- [ ] Task 6：OTel Host/Kubernetes receivers、持久化 checkpoint/queue。
- [ ] Task 7：Loki native OTLP 与 Elasticsearch exporter。
- [ ] Task 8：Logs UI 与设置页改造。
- [ ] Task 9：告警、AIOps、Incident 和外部跳转解耦。
- [ ] Task 10：灰度、历史查询、回滚、安装包和运维文档。
- [ ] Task 11：故障注入、E2E、性能与视觉验收。

## 变更记录

| 日期 | 变更人 | 变更内容 | 原因 |
| --- | --- | --- | --- |
| 2026-08-18 | Codex | 初始版本并进入开发 | 用户确认技术方案并要求实现 |

## 上线后复盘

- 实际指标：上线后四周补充。
- 是否达成：待复盘。
- 未达成原因：待复盘。
- 经验教训：待复盘。
- 下一步：评估 per-edge API Key、ES|QL、日志聚类和长期移除 Loki 兼容接口。
