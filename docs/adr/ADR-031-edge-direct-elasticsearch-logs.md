# ADR-031：Edge 直写外部 Elasticsearch

- 状态：已接受
- 日期：2026-08-18
- 作者：Codex
- 替代：不适用
- 关联：[ADR-029：拆分 Kubernetes Controller 与遥测数据面](./ADR-029-kubernetes-telemetry-data-plane-separation.md)

## 背景

Ongrid 当前把外部 Loki/Tempo endpoint 解析后直接下发给 Edge，内置后端才通过 Manager Nginx 鉴权反向代理。若新增 Elasticsearch 后把日志正文先转发给 Manager，会引入额外带宽、CPU、单点和背压故障域，并与 ADR-029 的遥测数据面分离原则相冲突。

同时，外部 Elasticsearch 需要独立写凭证，而普通插件快照有意不携带第三方密钥；写入链路和查询链路也可能使用不同网络地址与权限。

## 决策

1. 配置外部 Elasticsearch 时，Host Edge、Kubernetes Node Collector 和独立 Telemetry Gateway 直接连接 Elasticsearch，日志正文不经过 Manager、Frontier 或控制隧道。
2. Manager 只负责后端配置、加密凭证、目标 generation、Edge 应用状态、查询和审计。
3. Edge 只获得目标 data stream 的只写凭证；Manager 只保留只读查询凭证。两者独立轮换。
4. 内置 Loki 继续通过 Manager Nginx 精确 OTLP 写入口鉴权并代理；Manager Go 不接收日志正文。
5. 外部写入和内置 Loki 回滚都使用同一个 `otelcol-contrib` logs 流水线，不再让 Promtail成为长期回滚依赖。
6. 日志查询通过后端无关服务接口；浏览器、告警和 AIOps 不直接提交 Elasticsearch DSL。
7. 不允许永久生产双写。灰度期间，选定 Edge 在本地 Collector 中有界地同时写“当前权威后端”和“候选 ES”；查询仍只读权威后端。真实写探针全部通过后记录全局 `cutover_at`，再停止影子写并切换查询。
8. 全量切换只覆盖启用了 `logs` 插件的 Edge，并要求它们全部在线；否则拒绝移动全局查询时间线，避免离线 Edge 恢复后继续写旧后端。
9. 回滚采用对称流程：ES 仍是权威写入，Edge 影子写内置 Loki；只有全部 Loki 探针可通过 Manager 查询后才记录 `ended_at` 并切回 Loki。失败时保持 ES 权威，可重试失败 Edge。

## 备选方案

### 方案 A：Manager 中转全部日志

优点是 Edge 只访问一个地址，凭证集中。缺点是 Manager 承担所有日志带宽和背压，外部 ES 故障会影响控制面，且日志包需要额外编解码。未采用。

### 方案 B：部署中心 OTel Gateway 后再写 ES

优点是集中处理、凭证不下发到 Edge。缺点是新增高吞吐数据面组件、容量和高可用责任，仍增加一跳。可以作为严格出口网络场景的后续可选部署，不作为默认。

### 方案 C：继续只支持 Loki

改动最小，但无法补足成熟的正文全文检索、字段聚合和分页体验。未采用。

### 方案 D：Edge 永久双写 Loki 和 ES

便于对比和回滚，但会使存储成本翻倍，产生两套 delivery 语义和结果不一致。未采用；只允许 rollout assignment 生命周期内的有界影子双写。候选队列不允许反向阻塞权威 exporter，探针失败则禁止切换。

## 后果

### 正面影响

- Manager 控制面资源不随日志流量增长。
- 外部 ES 故障不会拖垮 Edge tunnel 和 Manager API。
- 写入延迟和网络跳数更少。
- OTel 同一采集流水线可以在 Loki 与 ES 间切换，checkpoint 不随后端变化。
- 写、读权限可独立收敛。

### 负面影响与权衡

- 每个 Edge 必须能访问外部 ES，网络和证书排障面扩大。
- 写 API Key 必须安全下发到 Edge；共享 Key 会扩大单 Edge 泄露影响面。
- Manager 连接测试不能证明 Edge 可达，必须增加真实 Edge 探针。
- 灰度和回滚预热期间存在短时双写成本；全量切换和回滚后的历史查询仍需按全局 `cutover_at/ended_at` 读取对应后端。
- 任一启用日志的 Edge 离线都会阻塞全量切换或回滚完成；这是用运维等待换取单一全局时间线的完整性。
- at-least-once 重试可能产生重复日志，不承诺 exactly-once。
