# quilscan-agent 核弹级代码质量审计

## 范围与结论

- 审计日期：2026-09-05；仓库：`quilscan-com/quilscan-agent`；固定 SHA：`8fd01701046d70853f11d98cd1a2b6bbd697c051`。
- 这是当前公开源码的**基线审计**，不是某个 PR 的新增回归判断。
- 重点覆盖 `cmd/agent`、命令分发、自动更新、状态持久化、Node/QClient/Agent 安装升级、WebSocket 及验证入口。业务源码未修改，未连接真实节点，未安装服务或执行升级。
- 结论：主要问题不是文件拆得不够细，而是同一节点资源有多个写入入口、同一 State 有多个全量写入者、同一安装事务有多套提交顺序。优先统一这些所有权；单纯拆文件会保留竞态和半完成状态。
- 本文 P1 表示应优先修复的安全/节点状态风险，P2 表示确定的验证或可维护性问题。均为既存基线问题，不能直接冒充本次变更合并阻断项。

## 确定缺陷与高置信风险

### AG-01 · P1 · 自动更新锁只保护 update_node，没有保护同一节点的其他破坏性动作

**证据：** [main.go:259](https://github.com/quilscan-com/quilscan-agent/blob/8fd01701046d70853f11d98cd1a2b6bbd697c051/cmd/agent/main.go#L259) 创建 gate 并传给 update handler；[controller.go:352](https://github.com/quilscan-com/quilscan-agent/blob/8fd01701046d70853f11d98cd1a2b6bbd697c051/internal/devnodeautoupdate/controller.go#L352) 后台直接执行该 handler；[main.go:355](https://github.com/quilscan-com/quilscan-agent/blob/8fd01701046d70853f11d98cd1a2b6bbd697c051/cmd/agent/main.go#L355)、[main.go:378](https://github.com/quilscan-com/quilscan-agent/blob/8fd01701046d70853f11d98cd1a2b6bbd697c051/cmd/agent/main.go#L378) 注册删除 store 和切换 source 时没有共享 gate。[delete_stores.go:64](https://github.com/quilscan-com/quilscan-agent/blob/8fd01701046d70853f11d98cd1a2b6bbd697c051/internal/actions/delete_stores.go#L64) 仅检查当时服务是否运行，然后移动 store；[update_node.go:257](https://github.com/quilscan-com/quilscan-agent/blob/8fd01701046d70853f11d98cd1a2b6bbd697c051/internal/actions/update_node.go#L257) 自动更新结束会启动服务。

**现有防护的边界：** WebSocket 手工命令彼此同步串行，Web UI 也有自动更新 busy 保护；但后台更新独立执行，后端 Command 只做白名单和转发，不能把 UI 保护当成节点资源互斥。

**触发条件：** Dev 自动更新停止服务、正在下载时，另一个客户端或 API 请求触发 `delete_node_store`。删除 handler 看到服务已停，开始备份；自动更新随后完成并启动节点。`switch_node_source`、`stop`、install/migrate 也不参加同一个互斥边界。

**影响：** 用户要求“删除 store 后保持停止”的语义可被后台更新打破；跨文件系统备份时甚至可能在复制/删除期间重新启动节点。切换 source 与更新也可能同时写同一 binary 与元数据。这是可从调度入口证明的交错风险，未在真实节点上复现数据损坏。

**最简改法：** 把现有 gate 提升为“节点变更 gate”，由所有会改变同一 service/binary/store 的入口共用；后台和手工路径都遵守。不要另造每动作一把锁。`stop` 在自动更新中究竟拒绝还是取消更新需明确产品语义，但不能静默互相覆盖。

**验证：** fake service + 阻塞 downloader，以 channel 固定“已停机、下载未完成”时序；此时派发删除/切源/stop，应被明确拒绝或按规定取消，不能执行交错的 start/move。用行为断言验证，单独 `-race` 不能发现这类逻辑竞态。

### AG-02 · P1 · State 的全量 Save 会把并发操作的新字段写回旧值

**证据：** [state.go:129](https://github.com/quilscan-com/quilscan-agent/blob/8fd01701046d70853f11d98cd1a2b6bbd697c051/internal/config/state.go#L129) 的 `SaveState` 只特别保留自动更新字段和较新的 manifest 字段；generation 只用于 remove/move，不随普通 Save 增长。[loop.go:346](https://github.com/quilscan-com/quilscan-agent/blob/8fd01701046d70853f11d98cd1a2b6bbd697c051/internal/reconcile/loop.go#L346) 先读快照，随后执行多项探测；[loop.go:549](https://github.com/quilscan-com/quilscan-agent/blob/8fd01701046d70853f11d98cd1a2b6bbd697c051/internal/reconcile/loop.go#L549) 保存前仅补 QClient 字段，再全量 Save。[update_node.go:186](https://github.com/quilscan-com/quilscan-agent/blob/8fd01701046d70853f11d98cd1a2b6bbd697c051/internal/actions/update_node.go#L186) 更新 NodeVersion 后也全量 Save。

**触发条件：** reconcile 读到旧 State → 节点升级保存新 NodeVersion → reconcile 将旧快照加上 PeerID/验证时间再保存。两次 Save 都持锁，也都成功，但锁没有覆盖整个读改写事务。

**影响：** 已更新的版本、生命周期时间等未受特判保护的字段会退回旧值，后端收到错误的状态。继续补 `preserveXxx` 只会制造越来越多的字段级例外。

**最简改法：** 已有 [UpdateState](https://github.com/quilscan-com/quilscan-agent/blob/8fd01701046d70853f11d98cd1a2b6bbd697c051/internal/config/state.go#L161) 可以直接复用。各调用方只在 callback 内修改自己拥有的字段；耗时采集在锁外完成。逐步移除长期持有快照后全量 Save 的 API 使用，不需要新数据库或事件框架。

**已运行验证：** 在临时目录复制该 SHA 的 `state.go`、`config.go`、go.mod/go.sum，添加单一测试并执行 `go test -race ./internal/config`，退出码 1：

```text
TestAuditStaleSnapshotLosesNodeVersion
stale reconciliation overwrote installed version: got "old", want new
```

复现逻辑如下；这是持久化逻辑丢更新，不是 Go 内存 data race：

```go
SaveState(p, &State{ConfigPath: "/node", NodeVersion: "old"})
stale, _ := LoadState(p)
fresh, _ := LoadState(p)
fresh.NodeVersion = "new"
SaveState(p, fresh)
stale.PeerID = "observed-peer"
SaveState(p, stale)
actual, _ := LoadState(p) // NodeVersion == "old"
```

修复后应保留这个交错测试，并追加两个不同字段的 UpdateState 均不丢失的断言。

### AG-04 · P1 · 发布 bundle 的提交不是原子的，QClient 还会先删除旧 sidecar

**证据：** [install.go:450](https://github.com/quilscan-com/quilscan-agent/blob/8fd01701046d70853f11d98cd1a2b6bbd697c051/internal/actions/install.go#L450) 先 stage sidecar，但随后先替换 binary，再逐个 rename sidecar；中途失败没有回滚。[install_qclient.go:229](https://github.com/quilscan-com/quilscan-agent/blob/8fd01701046d70853f11d98cd1a2b6bbd697c051/internal/actions/install_qclient.go#L229) 更直接：先删除旧签名/摘要，再 move binary 和 sidecars。[switch_node_source.go:95](https://github.com/quilscan-com/quilscan-agent/blob/8fd01701046d70853f11d98cd1a2b6bbd697c051/internal/actions/switch_node_source.go#L95) 已替换 dev binary 后才修改服务签名检查，后一步失败直接退出。

**触发条件：** binary 已替换后 sidecar rename、服务定义修改或 reload 失败；或 QClient binary move 失败而旧 sidecar 已删除。

**影响：** 返回失败时旧可运行集合已经不存在，磁盘上可能是“新 binary + 旧/缺失 sidecar + 旧 state”；切源失败还可能保持服务停止。当前文件级原子 rename 不能保证多文件操作的事务语义。

**最简改法：** 先完整准备并验证候选，保留旧 bundle 到提交成功；只在候选就绪后短暂停止服务。先实现统一的明确回滚流程覆盖 binary、sidecars、服务配置；只有平台服务约定允许时才考虑版本目录加单一指针切换。不要只把重复代码换个文件。

**验证：** 注入第 N 次 rename、服务配置写入或 reload 失败；断言旧 bundle 和运行策略仍可恢复，失败状态明确说明恢复结果。QClient move 失败时旧签名必须仍在。未做故障注入执行；证据为逐步错误返回路径。

### AG-05 · P2 · 公共声明的 Go 版本与实际标准库 API 不兼容，默认验证已失败

**证据：** [go.mod:3](https://github.com/quilscan-com/quilscan-agent/blob/8fd01701046d70853f11d98cd1a2b6bbd697c051/go.mod#L3) 声明 Go 1.22；[install_qclient.go:5](https://github.com/quilscan-com/quilscan-agent/blob/8fd01701046d70853f11d98cd1a2b6bbd697c051/internal/actions/install_qclient.go#L5) 引入 `crypto/sha3`，在 [222 行](https://github.com/quilscan-com/quilscan-agent/blob/8fd01701046d70853f11d98cd1a2b6bbd697c051/internal/actions/install_qclient.go#L222) 使用 Go 1.24 才提供的 `sha3.Sum256`。CI 虽选 Go 1.24，module 的语言/标准库版本声明仍为 1.22。

**已运行验证：** 本机 `go version go1.27.0 darwin/arm64`，在原审计 checkout 执行 `go test ./...`，退出码 1：

```text
internal/actions/install_qclient.go:222:14: sha3.Sum256 requires go1.24 or later (module is go1.22)
FAIL github.com/quilscan-com/quilscan-agent/internal/actions [build failed]
```

这是默认 go test 中版本检查导致的失败，不等同于所有新版本编译器都无法 build。`git ls-files '*_test.go'` 无结果，其他包输出 `[no test files]`；不能把这些输出称为行为测试通过。

**最简改法：** 若接受现有 CI 的最低 Go 1.24，直接将 go.mod 和支持文档对齐；只有确需 Go 1.22 才改用兼容实现。保留 AG-01/02/04 最小行为测试，比空测试门禁有意义。

**验证：** 在声明的最低 Go 版本执行 `go test ./...`，不通过关闭 vet 来掩盖版本契约。

## 结构简化建议（不是已验证故障）

### AG-S1 · 把采集结果变成有类型的状态，由单一位置生成 wire 字段

[loop.go](https://github.com/quilscan-com/quilscan-agent/blob/8fd01701046d70853f11d98cd1a2b6bbd697c051/internal/reconcile/loop.go) 有 1400 行，混合探测、持久化、版本判定、配置文件上报和 map 拼接；[controller.go](https://github.com/quilscan-com/quilscan-agent/blob/8fd01701046d70853f11d98cd1a2b6bbd697c051/internal/devnodeautoupdate/controller.go) 有 935 行。文件尺寸是基线事实，没有“本次跨过 1000 行”的证据。

NodeVersion/InstalledNodeVersion/current_node_version/node_info_version 的优先级反复出现在 reconcile、update、switch 和 install。建议先形成内部明确的 installed/observed/available 三类值，再由一个输出函数生成兼容的 wire 字段；这样可以删掉多套 patch 特判。必须保持现有协议键和优先级，不应趁审计建议擅改外部契约。验证应采用同一状态样本经安装、升级、reconcile 后输出一致的表格案例。

### AG-S2 · 保持命令串行，但不要让长命令占用 WebSocket 读循环

[ws/client.go:114](https://github.com/quilscan-com/quilscan-agent/blob/8fd01701046d70853f11d98cd1a2b6bbd697c051/internal/ws/client.go#L114) 在读 goroutine 同步调用 OnMessage；[main.go:460](https://github.com/quilscan-com/quilscan-agent/blob/8fd01701046d70853f11d98cd1a2b6bbd697c051/cmd/agent/main.go#L460) 同步 Dispatch，下载允许每个 artifact 最长 20 分钟。在这个期间 logs_off/stream_off 等控制帧不能被消费。

当前没有证据证明一定触发生产心跳超时：Agent 没设 read deadline，后端部署超时也未现场核查。因此这是明确的控制响应性和生命周期设计建议，不是已证实掉线事故。可以用有界队列与单一命令 worker 保持命令串行，由 WS 读循环立即处理连接控制；不能简单对每条消息 `go Dispatch`，否则扩大 AG-01。通过阻塞 fake handler 验证控制帧仍可处理、队列满时明确拒绝。

### AG-S3 · 安装逻辑的锁与验签复用应放在真正共享入口

[main.go:229](https://github.com/quilscan-com/quilscan-agent/blob/8fd01701046d70853f11d98cd1a2b6bbd697c051/cmd/agent/main.go#L229) 的 QClient mutex 只包住 startup/install 回调，显式 [install_qclient 注册入口](https://github.com/quilscan-com/quilscan-agent/blob/8fd01701046d70853f11d98cd1a2b6bbd697c051/cmd/agent/main.go#L403) 直接调用另一条 handler，绕过该锁。应在实际安装入口复用同一 gate。另 [binary_signature.go](https://github.com/quilscan-com/quilscan-agent/blob/8fd01701046d70853f11d98cd1a2b6bbd697c051/internal/actions/binary_signature.go) 已有通用 Ed25519 验证 helper，Agent 更新仍保留一份近乎相同实现；合并到已有 helper 即可，无需新增 crypto interface。

## 已确认的正面边界与盲点

- 动作使用注册白名单；未知 action 拒绝。默认 backend URL 为 WSS；不能把历史“任意 update_agent URL 无签名”结论继续套在此 SHA 上，当前在线自更新已验签并限制 URL 前缀。
- 主会话已完成该 Agent 图构建：63 files / 727 nodes / 5213 edges。本文证据来自直接源码和 rg 调用点核对；zg/graph 不被当作逐行全覆盖证明。
- 未对真实后端认证绑定、反向代理、systemd/launchd 现场权限、实际发布签名文件、节点二进制内部自验签或磁盘故障进行运行验证。未给出“无漏洞”“可直接部署”结论。
- 不建议一次整体重写：先修共享 gate 与 State 更新所有权，再处理安装事务，最后收敛 map/重复分支。每一步用失败时序验证行为守恒。

## 可重跑证据资产

- [State 故障复现器](repro-agent-state.py)：`python3 discuss/2026-09-05-code-quality-review/repro-agent-state.py --source /path/to/quilscan-agent`。只复制四个源码/模块文件到临时目录，强制 `GOWORK=off`，运行 `go test -race`；脚本退出 0 表示确认缺陷存在，不表示修复通过。
- [默认 Go 验证原始日志](agent-go-test.log)：退出 1，完整保留版本契约检查错误与各包无测试输出。
