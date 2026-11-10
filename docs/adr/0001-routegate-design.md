# ADR-0001: RouteGate 奠基设计(独立开源服务网关:完整设计与规划)

- 状态:Accepted(v0.4.1,2026-11-05;关键决策见 §18 决策记录,架构评审修订见「变更记录」)
- 来源:从生产环境集中式服务间网关的方向与实践提炼、泛化为独立开源项目
  - 设计动机:为多服务部署提供一个统一的强制执行点(身份验证 + 默认拒绝授权),承载东西向与边缘到后端全部流量
  - 前身实现为一次内部业务网关实践,本设计不继承其专有耦合,按全新项目标准设计
- 本文档为设计稿最新版(含 v0.4.1 架构评审修订,13 项发现全部处置)入库;后续架构修订以新 ADR 追加,不改写本文历史语义

> **变更记录**
> - v0.2:数据模型收敛为 8 张核心表;角色两级(超管/普通用户),变更审核流列 v1.5。
> - v0.3:明确全网统一入口(含边缘网关→后端一跳);证书认证提前为 v1 主身份(hub-and-spoke);授权规则支持通配/部分通配。
> - **v0.4(定稿)**:① 定名 **RouteGate**,仓库 `github.com/BadmintonCode/routegate-gateway`(当前 org);②
>   **删除 open 模式**,网关永远执行身份验证 + 策略评估(默认拒绝);③ 普通用户 v1 只读,
>   变更审核流列为 v1.5(视需求);④ 证书采用**逐服务钉证书**,且**支持在面板生成密钥对与
>   证书**(推荐浏览器端生成,私钥不经服务端);CA 模式移至 v1.5。
> - **v0.4.1(架构评审修订,2026-11-05)**:全量评审发现 1 个 P0、4 个 P1、若干 P2。已落入:
>   **① 独立健康检查监听**(P0——强制客户端证书与 LB/Docker/K8s 探针在 TLS 层冲突):
>   `ROUTEGATE_HEALTH_LISTEN`(默认 `:8082`,地址/端口可配置,可独立关闭),`:8080` 不再
>   承载任何健康端点,`/__routes` 调试端点移入 Admin API(会话认证保护)。
>   **② 吊销×DB 故障窗口**:面板生成证书默认有效期 12 个月→**90 天**;运维预案写明
>   「紧急吊销的前置是 MySQL 可用」;v1.5 预留每实例本地紧急吊销指令。
>   **③ MySQL 可用性**:「跨重启生存能力以 MySQL 为上界」的风险链完整写入 §14 与部署
>   文档,MySQL HA + DSN 指向稳定端点定位为**强烈建议**(用户决策:不设硬性准入门槛)。
>   **④ DB 账号模型**(用户决策:**单 DSN 不分离**):双 DSN 最小权限提案被否,数据面
>   持全权为已知接受风险;缓解为 MySQL 仅对网关网络可达,姿态披露于 §11.9,未来可
>   选增 `ROUTEGATE_ADMIN_DSN` 收紧。
>   **⑤ Admin API 乐观并发**(用户确认:v1 就做):PUT/DELETE 携带 `expected_revision`,
>   冲突返回 409 + 最新配置;v1.5 审批复用该校验。
>   **⑥ 停机语义**(用户决策:**v1 不做排水**):SIGTERM 即退出;调用方以重试+幂等键
>   自保,文档明示;优雅排水列为候补。
>   **⑦ 直改 DB 自愈**(用户确认:强刷+bump CLI):每 N tick(默认 10,可配)强制全量
>   重载,不依赖 revision;新增 `routegate config bump` 子命令与 break-glass 文档流程。
>   **⑧ 上游连接策略**(用户确认:keepalive+DNS 缓存):默认开启连接复用 + DNS 缓存
>   TTL 5s,保留重平衡特性;配置项可回退;M1 设压测门槛。
>   **⑨ 部署拓扑**(用户决策:仅一句话级建议):生产建议管理面独立部署,admin 端口
>   永不与数据面同网暴露/映射公网;不展开拓扑图。
>   **⑩ 协议矩阵**(未收到答复,按推荐执行):§5.2 新增协议支持矩阵;SSE FlushInterval
>   列入 M1 验收;WebSocket 透传标实验性;上游侧 v1 不承诺 h2。
>   **⑪ 路由目标自环校验**(用户确认:加):targets 不得等于本实例任一监听地址(含
>   localhost/127.0.0.1),加载器与 Admin API 共用该规则。
>   **⑫ instances 陈旧行**(未收到答复,按推荐执行):Worker 删除超 10 分钟未更新行 +
>   面板按新鲜度判定存活,双保险。
>   **⑬ 策略矩阵视角**(用户确认:service 聚合):面板矩阵按 caller × 被调服务聚合
>   (多路由归列、格内分组),列表接口支持 `group_by=service`;数据模型不动。
>
>   **处置汇总**:13 项中 11 项按推荐落入设计;2 项按用户决策降级/否决(P1-4 DB 账号
>   不分离、P1-5 优雅停机 v1 不做),均已带风险披露记录;P2-10、P2-12 未收到答复按
>   推荐执行。

---

## 1. 项目定位

### 1.1 一句话定位

**RouteGate** 是一个以 MySQL 为唯一控制面依赖的服务网关:它是系统内**所有服务间调用的统一入口与强制点**——包括服务之间的东西向调用,以及对客边缘网关(公网入口层)调用后端服务的这一跳;路径路由 + 默认拒绝的接口级授权策略(支持通配)+ 证书(mTLS)工作负载身份,自带管理面板,单二进制部署,无 etcd / 无控制面集群 / 无 Redis 强依赖。

- 仓库:`github.com/BadmintonCode/routegate-gateway`
- 语言/形态:Go 单二进制 `routegate`(数据面 + 管理面 + 健康检查三监听,内嵌面板)
- 许可:Apache-2.0

### 1.2 能力范围(2026-11-05 与用户对齐)

1. **全网统一入口**:所有服务之间的调用都经过本网关;对客那一层的网关(如 APISIX/边缘网关)调用后端服务,同样经过本网关——边缘网关在模型中就是一个持有身份凭证的普通调用方。网关因此是**全流量的关键路径**(HA 要求见 §14)。
2. **证书认证、中心化信任(hub-and-spoke)**:网关与每个服务做证书认证;每个服务只需要维护「自己 ↔ 网关」这一条信任关系,服务两两之间不需要互配证书。所有服务的信任材料(证书公体)集中维护在网关控制面,并支持在面板直接生成密钥对与证书(§6.3.2)。
3. **接口级授权矩阵**:在网关里维护「每个服务能够访问哪些服务的哪些接口」;规则支持**通配与部分通配**(如 `/v1/*`、`/v1/users/*`、`/v1/*/detail`),语义见 §6.1。

### 1.3 目标

1. **独立开源项目**:从生产环境业务网关的实践泛化设计,不携带旧系统专有耦合,按全新项目标准设计(不为旧表、旧格式、旧部署做兼容)。
2. **配置面 = MySQL**:路由、策略、身份、管理账号全部存 MySQL;部署一个网关只需要「一个二进制 + 一个 MySQL 库」。
3. **周期同步 Worker**:网关进程内置 Worker,按环境变量配置的间隔(默认 **60 秒**)从 MySQL 加载配置快照,原子热更新;**热路径(请求处理)完全不访问数据库**。
4. **管理面**:Admin API + Web 管理面板 + 两级管理账号(超级管理员 / 普通用户)。
5. **落实 ADR-0078 的核心语义**:默认拒绝的 `caller → target → operation` 策略、可验证工作负载身份(网关不保管私钥)、访问决策审计、fail-closed。

### 1.4 非目标(明确不做)

- 不做完整 API 管理平台(无限流插件市场、Lua 脚本、Developer Portal、计费)。
- 不做服务网格 sidecar(不做 gRPC 全覆盖;东西向只覆盖 HTTP/HTTPS)。
- 不做端用户认证:对客层的用户登录/会话仍属于边缘网关与认证服务;本网关只负责验证「调用方服务」的身份,并按信任规则转发端用户上下文(§6.3.4)。
- **不做无鉴权模式**:网关永远执行身份验证与策略评估(默认拒绝);没有「仅路由转发」的旁路。
- v1 不做变更审核流(普通用户提交接口变更 → 超管审批);v1.5 视需求实现(§8.5、§15.1)。
- v1 不做控制面向网关的实时推送(拉模型,扩展位见 §3.6)。
- 不承诺替代 APISIX:边缘(公网入口)职责仍属于边缘网关;RouteGate 是其身后**内层的统一服务网关**。

### 1.5 与现有实现的继承关系

| 现有 `business-gateway-service` | 开源版处理 |
| --- | --- |
| 显式前缀路由、strip_prefix、多目标轮询、周期刷新、原子快照、requestId、`/__health` `/__ready`、请求头清洗、拒绝 catch-all 前缀 | **保留并泛化**(核心资产,约 650 行,直接移植改造) |
| 旧版单路由表(单表 prefix + 逗号分隔 target_host) | **全新设计**(§4),不做旧表兼容 |
| 专有统一信封(强制 HTTP 200 + body code)、`/console` 特判、字典多语言改写、附件直通等旧系统耦合 | **剥离**;响应归一化做成可选的「响应 Profile」插件位,开源默认透传 |
| 定制 EdDSA service-context 二进制格式 | **替换**:v1 主身份 = 客户端证书(mTLS);标准 JWT(EdDSA)为可选备选 |
| 无管理面,路由靠手工 SQL | **新增 Admin API + 面板 + 账号体系**(本项目的主要增量) |

---

## 2. 总体架构

### 2.1 三平面拓扑

```text
                    ┌─────────────────────────────────────────────┐
                    │              管理平面(仅管理网络)              │
                    │   浏览器 ──► Admin 面板(SPA,静态资源)          │
                    │                │ 同源                         │
                    │                ▼                             │
                    │        Admin API :8081  ──读写──►  MySQL      │
                    └─────────────────────────────────────────────┘
                                     │ 写配置(事务内递增 config_revision)
                                     ▼
   ┌─────────────────────────── MySQL(唯一控制面事实源)──────────────────┐
   │  settings / routes / policies / identities                        │
   │  users / sessions / admin_audit / instances                       │
   │  (access_log 可选)                                                 │
   └───────────────────────────────┬───────────────────────────────────┘
                                   │ 只读轮询(Worker,默认 60s)
 调用方(全部经本网关,mTLS 客户端证书)           ▼
 公网 ─► 边缘网关(对客层,用户认证在这里)──cert:edge-gateway──┐
 服务 A ──────────────────────────cert:service-a─────────────┤
 服务 C ──────────────────────────cert:service-c─────────────▼
                                              ┌──────────────┐ mTLS/TLS ──► 上游 A :8080
                                              │ RouteGate    │ ────────────► 上游 B :8080
                                              │  N 实例无状态  │  (专用私网)
                                              └──────────────┘
                                              热路径不访问 DB;快照常驻内存
```

要点:**边缘网关只是调用方之一**——它对用户做认证(登录、会话),然后以自己的服务身份(证书 `edge-gateway`)把请求送进 RouteGate;RouteGate 验证边缘网关的服务身份、按策略放行、再把请求转给后端服务。端用户上下文只在调用方身份 = 可信边缘网关时按规则转发(§6.3.4)。

### 2.2 进程与监听模型

**单二进制 `routegate`,三监听,可独立开关:**

- `:8080` 数据面(proxy listener)——mTLS 服务端证书 + 要求客户端证书;路由、策略、转发、访问日志。**不承载任何健康检查端点**(证书强制与探针不兼容,见下)。
- `:8081` 管理面(admin listener)——Admin API + 面板静态资源,默认只绑管理网络(容器部署时绑定管理网络,不映射公网)。
- `:8082` 健康检查(health listener)——**明文 HTTP,无证书要求**,仅内部网络可达;只挂 `GET /__health`、`GET /__ready`、`GET /__status`、`GET /__metrics` 四个只读端点(`/__metrics` 为 Prometheus 文本暴露,§10:仅只读计数器与状态值,不含请求数据,scrape 路径无 DB I/O),不挂任何可变更/可转发语义,最小化攻击面。地址与端口由 `ROUTEGATE_HEALTH_LISTEN` 配置(默认 `:8082`),`ROUTEGATE_HEALTH_ENABLED=false` 可整体关闭(关闭后 Docker HEALTHCHECK/K8s 探针需自行另配,文档警示)。三个监听地址启动时互斥校验,冲突即 fatal。

> **为什么健康检查必须独立监听**(v0.4.1 评审 P0):TLS 握手发生在 HTTP 之前,数据面
> `CLIENT_AUTH=REQUIRED` 下,不带证书的探针(LB/APISIX upstream check、Docker
> HEALTHCHECK、K8s liveness/readiness probe)在握手阶段即被掐断,路径级豁免在 TLS 层
> 不存在。若健康端点留在 `:8080`,部署起来即全量摘除。独立监听同时让 `:8080` 保持
> 「无例外证书强制」的纯净语义。

- 小规模部署:一个进程同时开三个监听(`ROUTEGATE_ADMIN_ENABLED=true`)。
- 大规模部署:数据面横向扩 N 实例,管理面单独起 1–2 个实例(`ROUTEGATE_PROXY_ENABLED=false`),两者共用同一个库;健康监听随每个数据面实例存在,供 LB/编排系统探测。

子命令:`routegate serve`(默认)、`routegate migrate`(执行内嵌迁移)、`routegate config bump`(break-glass:直改 DB 后立即递增 `config_revision`,改动下个周期生效)、`routegate version`。

### 2.3 技术栈

| 层 | 选型 | 理由 |
| --- | --- | --- |
| 语言 | Go ≥ 1.23 | 继承现有实现,零框架(`net/http` + `httputil.ReverseProxy`),TLS/mTLS 为标准库能力 |
| 数据库驱动 | `github.com/go-sql-driver/mysql` | 现有依赖,唯一必须外部依赖 |
| 密码哈希 | `golang.org/x/crypto/argon2`(Argon2id) | 准标准库 |
| 身份令牌(可选备选) | `github.com/golang-jwt/jwt/v5`(EdDSA) | 标准 JWT,生态互通 |
| 管理面板 | React 18 + TypeScript + Vite + Ant Design 5(已确认) | toB 管理后台成熟方案;构建产物 `go:embed` 进二进制 |
| 面板证书生成 | WebCrypto + PKI.js(纯前端依赖) | 浏览器端生成密钥对与自签证书,私钥不经服务端(§6.3.2) |
| 迁移工具 | 自带内嵌迁移(启动时按序执行 `migrations/*.sql`,记录 `schema_migrations`) | 免额外 CLI 依赖 |

Go 侧依赖白名单冻结为:mysql driver、x/crypto、golang-jwt(仅因 JWT 备选存在)。其他一律不引入;前端依赖允许 PKI.js(证书生成所需)。

---

## 3. 配置同步 Worker(核心机制)

### 3.1 行为定义

```text
启动:
  连接 MySQL(指数退避重试 1s→30s,直至成功或 GATEWAY_BOOT_MAX_WAIT 超时 fatal)
  执行内嵌迁移(可关:GATEWAY_AUTO_MIGRATE=false)
  首次全量加载快照(含身份证书材料)→ 成功前 /__ready = 503
  (无快照时快照为空 → 等价于全部拒绝,fail-closed,安全)

运行(每 tick):
  1. SELECT value FROM settings WHERE name='config_revision'   ← 极轻量探测
  2. revision == 已加载 revision?
       是 → 记录心跳,继续——**但每 N tick 强制一次全量重载(自愈刷新)**
            (N 由 ROUTEGATE_FORCE_RELOAD_EVERY 配置,默认 10;不依赖 revision 判断,
             兜住「直改 DB 忘 bump」与 revision 计数异常两类静默失效)
       否 → 全量加载(见 3.2)→ 校验 → 构建新快照 → 原子替换
  3. 附带职责:清理过期会话(sessions);upsert instances 心跳;
     标记已过期策略(供面板提示;不自动删除)
```

### 3.2 全量加载与原子替换

- 单事务一致性读:`START TRANSACTION WITH CONSISTENT SNAPSHOT` 内一次读出 routes + policies + identities(数据量为「几十路由 × 几百策略 × 几十证书」级,毫秒级完成)。
- 证书材料在加载时**预解析**:PEM → `x509.Certificate` / 公钥,解析失败的行丢弃并计数;`**` 位于 pattern 中间的非法策略、悬空 route 引用同理。
- 加载器做**校验与净化**:非法前缀(空、`/`、含通配符)、targets JSON 解析失败、**targets 自环(host:port 等于本实例任一监听地址,含 `localhost`/`127.0.0.1`,v0.4.1:防配置错误导致转发无限自环打死实例)**、重复条目 → 丢弃该行并计数上报(`routegate_config_invalid_rows_total`),**不让单行脏数据阻塞整份快照**。
- 新快照完整构建成功后才替换:`atomic.Pointer[snapshot.Snapshot]`(或沿用现实现的 `sync.RWMutex` 方案,二选一,以压测为准);替换瞬间完成,无锁争用窗口。
- 快照内容:按前缀长度降序的路由表(含各自 targets)、通配策略匹配器、身份验证材料(证书指纹表、JWT 公钥表)、快照 revision。

### 3.3 环境变量(用户核心诉求)

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `ROUTEGATE_CONFIG_SYNC_INTERVAL` | `60s` | 同步间隔,Go duration(`30s`/`2m`/`1h` 均可),下限 `1s` |
| `ROUTEGATE_DB_DSN` | 必填 | MySQL DSN,含 `parseTime=true&timeout=5s&readTimeout=10s` |
| `ROUTEGATE_READY_STALE_AFTER` | `3 × SYNC_INTERVAL` | 超过该时长未成功同步 → `/__ready` 503 |
| `ROUTEGATE_DRAIN_TIMEOUT` | `20s` | SIGTERM 排水宽限:在途请求排空的上限,超时立即关闭(§14) |
| `ROUTEGATE_INSTANCE_ID` | hostname | 心跳与访问日志中的实例标识 |
| `ROUTEGATE_FORCE_RELOAD_EVERY` | `10` | 每 N tick 强制全量重载(自愈刷新;`1` = 每次 tick 都全量) |

### 3.4 故障语义(明确表)

| 场景 | 行为 |
| --- | --- |
| 启动时 DB 不可达 | 退避重试;始终无快照 → 快照为空 = 全拒绝,`/__ready` 503,不 fatal(便于依赖启动顺序编排;`ROUTEGATE_BOOT_MAX_WAIT` 可改为 fatal) |
| 运行中 DB 不可达 | **继续用最后一份快照服务流量**(热路径无 DB);`/__ready` 在 stale 后转 503 → 上游 LB 摘除;恢复后自动追平 |
| 单条配置脏数据 | 丢弃该行 + 计数,其余正常生效 |
| 加载失败(事务/网络错误) | 保留旧快照;连续失败计数 + 日志 + 指标 |

### 3.5 心跳与实例可见性

每实例每 tick(或每 30s,取小者)upsert `instances` 表:实例 ID、二进制版本、已加载 revision、最近成功同步时间、启动时间。面板的「系统状态」页据此展示 **「配置已传播到 N/M 实例」**(对比 settings.config_revision 与各实例 loaded revision)。

陈旧行治理(v0.4.1,双保险):Worker 每 tick 顺带 `DELETE` 超过 10 分钟未更新的行(实例异常死亡后不留幽灵行);面板侧「存活实例」一律按新鲜度(最近心跳 ≤ 3×interval)判定,不裸数表行数——覆盖进程假死(tick 卡住但行还在更新)的极端情况。

副作用治理补记(2026-11-08,并行变更入库):sessions/instances/admin_audit 热查询在迁移 0008 补齐索引,sessions/instances 的 GC 与管理审计列表不再全表扫描;admin_audit 与 alert_events 新增 **90 天保留期 janitor** 定期清理——需要留存更久审计的运维方应在到期前先行导出。

### 3.6 与实时化的关系(扩展位)

v1 纯拉模型;如未来要秒级生效,可在 Admin API 上加 `POST /admin/api/v1/config/hint`,网关收到 hint 后将下一次 tick 提前(仍以 DB 为事实源,推送只是「提前拉」的触发器,不携带数据)。接口位保留,实现推迟。

---

## 4. 数据模型(MySQL Schema)

**全新项目、全新设计**:8 张核心表 + 1 张可选日志表,不做任何旧系统兼容映射。utf8mb4 / InnoDB;配置表不各带 revision,统一由 `settings.config_revision` 在**同一事务内**随写操作递增——这是「改一次配置,全网关下一个周期必见」的保证。

### 4.1 表清单

| 表 | 职责 | 写入方 |
| --- | --- | --- |
| `settings` | 键值设置;其中 `config_revision` 单调递增,是同步信号 | Admin API(事务内) |
| `routes` | 入口路由:前缀、上游目标列表(JSON)、转发选项 | 超管 |
| `policies` | 授权规则:caller × 路由 × 操作(支持通配)→ allow/deny | 超管 |
| `identities` | 工作负载身份(**只存证书公体/公钥**,永不存私钥) | 超管 |
| `users` | 面板账号(超级管理员 / 普通用户) | 超管(自举) |
| `sessions` | 面板会话 | Admin API;Worker 清理过期 |
| `admin_audit` | 管理操作审计(追加写) | Admin API(与写操作同事务) |
| `instances` | 网关实例心跳 | 数据面 Worker |
| `access_log`(可选) | 访问决策日志(MySQL sink 时) | 数据面批量写入器 |
| `schema_migrations` | 迁移版本(工具表) | migrate |

### 4.2 关键表结构(DDL 级描述)

```sql
-- 路由:显式前缀白名单(继承现实现约束:拒绝 ''、'/'、含通配符的前缀)
-- 上游目标内联为 JSON:新项目追求极简,几十条路由的规模无需单独目标表;
-- 若未来需要独立目标管理,再拆表迁移(成本可控)
routes (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  name VARCHAR(128) NOT NULL UNIQUE,
  path_prefix VARCHAR(256) NOT NULL UNIQUE,   -- 最长前缀优先匹配;路由层不支持通配(确定性)
  methods VARCHAR(128) NULL,                  -- NULL=全部;'GET,POST'
  targets JSON NOT NULL,                      -- [{"host":"svc-a","port":8080,"weight":1},…]
  strip_prefix TINYINT(1) NOT NULL DEFAULT 0,
  enabled TINYINT(1) NOT NULL DEFAULT 1,
  remark VARCHAR(255) NOT NULL DEFAULT '',
  created_at / updated_at DATETIME
)

-- 授权规则(能力范围 3:哪个服务能访问哪条路由的哪些接口;支持通配,语义见 §6.1)
policies (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  route_id BIGINT UNSIGNED NOT NULL,          -- 目标路由;删除路由前须先删其策略
  caller VARCHAR(128) NOT NULL,               -- = identities.name;'*' 仅限临时规则(须带 expires_at)
  method VARCHAR(16) NOT NULL DEFAULT '*',    -- '*','GET','POST',...
  path_pattern VARCHAR(256) NOT NULL DEFAULT '/**',
                                              -- 前缀剥离后的子路径模式;'/**'=整条路由
                                              -- 支持:'/v1/users/*'(段通配)、
                                              --       '/v1/*/detail'(部分通配)、
                                              --       '/v1/users/**'(子树)、
                                              --       '/**'(全部,面板高亮提示)
  effect ENUM('allow','deny') NOT NULL,
  expires_at DATETIME NULL,                   -- caller='*' 或 pattern='/**' 时强制非空
  enabled TINYINT(1) NOT NULL DEFAULT 1,
  created_by BIGINT UNSIGNED NOT NULL,        -- 超管 ID
  remark VARCHAR(255) NOT NULL DEFAULT '',
  created_at / updated_at DATETIME,
  INDEX idx_route_caller (route_id, caller, enabled)
)

-- 工作负载身份:只存验证材料,永不存私钥(ADR-0078 红线;能力范围 2)
identities (
  id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  name VARCHAR(128) NOT NULL,                 -- 调用方主体名(策略 caller 引用它)
  key_id VARCHAR(64) NOT NULL,                -- 轮换标识:证书 SHA-256 指纹;JWT=自定义 kid
  credential_type ENUM('mtls_cert','ed25519') NOT NULL DEFAULT 'mtls_cert',
                                              -- v1.5 增 'mtls_ca'(内部 CA 模式)
  public_material TEXT NOT NULL,              -- PEM:叶子证书(自签或外部签发均可)/ Ed25519 公钥
  source ENUM('upload','generated') NOT NULL DEFAULT 'upload',  -- 面板生成(§6.3.2)或外部上传
  status ENUM('active','revoked') NOT NULL DEFAULT 'active',
  not_before DATETIME NULL, not_after DATETIME NULL,   -- 证书有效期,加载时校验
  revoked_at DATETIME NULL, revoke_reason VARCHAR(255) NOT NULL DEFAULT '',
  created_by BIGINT UNSIGNED NOT NULL,
  remark VARCHAR(255) NOT NULL DEFAULT '',
  created_at / updated_at DATETIME,
  UNIQUE KEY uk_name_kid (name, key_id),
  INDEX idx_name_status (name, status)
)

-- 其余表(列要点)
settings   (name VARCHAR(64) PK, value JSON, updated_at)   -- 含 'config_revision'
users      (id, username UNIQUE, password_hash ARGON2id PHC,
            display_name, role ENUM('superadmin','user'), status ENUM('active','disabled'),
            failed_login_count, locked_until, must_change_password,
            last_login_at, last_login_ip, created_at/updated_at)
sessions   (id, user_id, token_hash CHAR(64) UNIQUE, created_at, expires_at,
            absolute_expires_at, ip, user_agent, revoked_at NULL)
admin_audit(id, actor_id, actor_name, action, resource_type, resource_id,
            before JSON, after JSON, request_id, ip, created_at)   -- 只增不改
instances  (instance_id PK, version, loaded_revision, started_at,
            last_sync_at, last_sync_ok, updated_at)                 -- Worker upsert
```

---

## 5. 请求处理管线(数据面)

```text
请求(TLS 握手:网关出示服务端证书,并要求客户端证书)
 → [1] requestId 中间件(沿用现实现:校验/复用 X-Request-Id,缺省生成,透传 traceparent)
 → [2] 调用方身份解析(永远执行,无旁路):
        证书模式(主):客户端证书 → 指纹匹配 identities → caller;失败 → 401
        JWT 备选:Bearer 令牌 → kid 选公钥验签(iss/aud/iat/exp≤300s/jti)→ caller
 → [3] 路由匹配:最长显式前缀;未匹配 → route_not_found
 → [4] 策略评估(永远执行):(caller, route, method, 通配匹配子路径)→ allow / deny / 默认拒绝
        拒绝 → 403;端用户上下文仅当 caller ∈ 可信转发名单时转发(§6.3.4)
 → [5] 目标选择:targets 中加权轮询;上游连接 **keep-alive 复用 + DNS 缓存(TTL 默认 5s)**
        (v0.4.1:取代「每请求新建连接+重解析」的旧语义;副本扩缩容最多延迟 TTL 后重新均衡;
         `ROUTEGATE_UPSTREAM_KEEPALIVE=false` 可回退旧行为)
 → [6] 转发:strip_prefix;清洗全部入站身份/代理头,重建:
        X-Gateway-Caller / X-Gateway-Request-Id / X-Gateway-Cert-Subject 等
        (2026-11-08 勘误:v1 实际不重建 X-Gateway-Cert-Subject——入站头照旧清洗,
         重建的仅 X-Gateway-Caller / X-Gateway-Request-Id,因 identity.Resolve 只携带
         caller 名;相关代码注释已由并行修复勘正)
 → [7] 上游调用:可选 TLS(网关出示自己的证书);拨号 3s / 响应头 60s / 可选整体超时;
        被动健康检查(连续失败 N 次 → 熔断 cooldown 30s)
        (2026-11-08 勘误:v1 未实现被动健康检查,pickTarget 为纯加权轮询;上行所述
         「连续失败 N 次 → 熔断 cooldown」列为后续候选,不作为已落地特性)
 → [8] 响应:默认透传;可选 Response Profile 做信封归一化(预留插件位)
 → [9] 访问决策日志(§6.5):异步、缓冲、可丢不阻塞
```

### 5.1 身份验证模式

`ROUTEGATE_CLIENT_AUTH`(默认 `cert`)——三种取值都**永远执行策略评估、默认拒绝**,区别仅在身份凭据形式:

| 取值 | 行为 |
| --- | --- |
| `cert`(默认) | 客户端证书(指纹匹配 identities) |
| `cert_or_jwt` | 证书或 JWT 任一通过(过渡期 / 少数无证书环境) |
| `jwt` | 仅 JWT(本地开发、CI 等无 mTLS 环境) |

### 5.2 协议支持矩阵(v0.4.1)

| 协议/形态 | 支持级别 | 说明与实现注意点 |
| --- | --- | --- |
| HTTP/1.1(含 chunked) | **完整支持** | 核心场景;所有行为语义以 HTTP/1.1 为准 |
| HTTP/2 | 客户端侧支持 / 上游侧 v1 不承诺 | 客户端 h2 over TLS 由 Go 标准库随 ALPN 自动支持;网关→上游 v1 默认 HTTP/1.1,不承诺 h2/h2c(后续版本评估) |
| SSE / 流式响应 | 支持,**必须实现 FlushInterval** | ReverseProxy 默认缓冲响应会把 SSE 卡成攒批;`FlushInterval` 必须设置(如 -1 或 100ms 级)——列入 M1 验收 |
| WebSocket(Upgrade) | 透传,标注实验性 | 升级请求按 GET+路径走策略匹配后透传 Upgrade;v1 不做专项测试保证,文档标注实验性 |
| gRPC | **不支持**(非目标) | 依赖上游 HTTP/2 全链路;已列 §1.4 非目标 |

---

## 6. 身份、策略与审计(落实 ADR-0078)

### 6.1 授权矩阵与通配语义(能力范围 3)

**匹配键:`caller × route × (method, 子路径模式)`**——即「哪个服务,能访问哪条路由(某服务)的哪些接口」。

路径模式语法(应用于 strip_prefix 之后的子路径;pattern 与路径都先做 §6.2 归一化):

| 模式 | 匹配 | 典型用途 |
| --- | --- | --- |
| `/v1/users` | 仅该路径 | 精确接口 |
| `/v1/users/*` | 其下**恰好一段**:`/v1/users/123` | 资源级接口 |
| `/v1/*` `/detail` | 中段通配:`/v1/orders/detail`、`/v1/wallets/detail` | 部分通配 |
| `/v1/users/**` | 其下**一或多段**(子树) | 一组接口 |
| `/**` | 整条路由 | 整服务授权(面板高亮 + 建议设过期) |

**规则与约束:**

- 段级匹配:pattern 与路径按 `/` 分段比对;`*` = 恰好一段;`**` = 一或多段,**只允许出现在 pattern 末尾**(保证匹配确定性);字面段内不允许出现 `*`/`?`(避免歧义)。
- 尾斜线归一:两侧统一去尾斜线后再比对(`/v1/users/` ≡ `/v1/users`)。
- **评估顺序固定:`deny > allow > 默认拒绝`**;任一匹配的 deny 即拒绝;无任何匹配 allow 即拒绝(fail-closed)。
- 通配的纪律(对应 ADR-0078 决策 2):`caller='*'`(任意调用方)与 `pattern='/**'`(整路由)的规则**强制要求 `expires_at`**,过期即失效;面板对临期(≤7 天)规则高亮催办。段级 `*` / 子树 `**` 属于常规表达,不强制过期。
- 策略快照随配置同步加载,评估纯内存;快照内按 route_id 分桶 + 段树预编译,单次评估与规则数近线性。
- 路由方法回退(2026-11-08 勘误,与数据面实现对齐):路由按最长前缀优先匹配,但仅当该路由的 methods 集合允许请求方法时才算命中;更具体前缀的方法不匹配**不产生 405**,匹配继续向更短前缀回退,全部未命中才返回 404 `route_not_found`。

### 6.2 路径归一化(匹配安全的前提)

单一归一化点:进入路由匹配前对 `r.URL.Path` 执行 RFC 3986 `remove_dot_segments`(消除 `.`/`..` 段),解码百分号编码一次;查询串不参与匹配;归一化结果同时用于路由匹配与策略匹配,杜绝「匹配用原始路径、转发用归一化路径」的错位绕过。模糊测试覆盖 `/../`、双重编码、尾斜线、空段等边界。

### 6.3 证书认证(能力范围 2,v1 主身份机制)

#### 6.3.1 信任模型:hub-and-spoke

```text
        service-a ──cert──┐
        service-b ──cert──┤     N 个服务 = N 份信任材料,全部集中在网关
        edge-gw   ──cert──┤     服务之间两两不需要任何证书关系
        service-c ──cert──┘
                      ▼
                 RouteGate(验所有来方证书;转发时可出示自己的证书)
```

- 每个服务只需:① 持有一张客户端证书(私钥在服务自己手里,网关永远不保管私钥);② (可选)信任网关的服务端证书以验证「对端确实是网关」。
- **网关不保管任何服务私钥**(ADR-0078 红线;面板生成的私钥同样只在交付瞬间出现,见 §6.3.2)。
- 服务间两两互信的需求被网关吸收:调用方只认网关,上游只认网关。

#### 6.3.2 证书的三种来源(v1:钉证书 + 面板生成;内部 CA 移至 v1.5)

| 来源 | 流程 | 私钥去向 |
| --- | --- | --- |
| **A. 上传已有证书**(外部签发/自签) | 服务方自行生成密钥对与证书 → 超管在面板上传 **PEM 证书公体**(预检:解析/有效期/指纹) | 服务方自持,网关不接触 |
| **B. 面板生成**(推荐给新接入服务) | 见下 | **只在交付瞬间出现,不落库不落盘** |
| C. 内部 CA(v1.5 规划) | 面板管 CA 证书,签发叶子证书,SAN 绑定身份名 | 服务方自持 |

**面板生成流程(B)**:

1. 超管在面板输入身份名(`service-a`)+ 有效期(默认 **90 天**;v0.4.1 由 12 个月下调,用于压缩吊销机制失效时的泄露窗口,见下「吊销语义与故障窗口」)。
2. **推荐路径:浏览器端生成**(WebCrypto 生成 ECDSA P-256 密钥对 + PKI.js 组装自签证书)——私钥全程不出浏览器;面板展示「一次性下载」(PEM key + cert,或 PKCS#12),关闭页面后服务端无任何私钥痕迹。
3. 备选路径:Admin API 服务端生成,私钥在**响应中返回且仅此一次**(响应不缓存、不写日志、审计只记 `identity.generated` 事件与证书指纹);文档明示该路径私钥经过服务端内存一次,安全要求高时用浏览器端生成。
4. 落库的只有**证书公体**(`source='generated'`,key_id=指纹);下个同步周期全网生效。
5. 生成证书的 SAN 固定为 `URI: https://routegate.local/identities/<name>`,与 identities.name 一致性校验对三种来源统一执行,防「合法证书冒用他人名字」。

- 证书 → caller 映射:按 SHA-256 指纹直接对应 identity 行(v1 不做 CA 链解析)。
- 校验内容:指纹精确匹配 + 有效期(not_before/not_after)+ 未吊销(active 行才进快照)+ key 品质(ECDSA P-256 / Ed25519 / RSA ≥2048)。
- **轮换**:同一 `name` 下生成/上传新证书(新 key_id)→ 下个周期双证书并存 → 服务切换 → 吊销旧行;全程不中断。
- **吊销语义与故障窗口**(v0.4.1 评审 P1):吊销经 DB 生效,正常传播 ≤ 1 个同步周期;但 **DB 宕机期间 fail-static 使吊销无法传播**——运维预案明示「紧急吊销的前置是 MySQL 可用」(先恢复 DB,再吊销);第一道缓解是短有效期(生成默认 90 天)。v1.5 预留**每实例本地紧急指令**(经管理端点从本实例快照立即丢弃指定 identity;逐实例执行,非广播方案,仅作 DB 长期不可用时的最后手段)。
- TLS 层配置:数据面 listener `CLIENT_AUTH=REQUIRED`;TLS 1.3;鼓励调用方连接复用/会话恢复,摊薄握手成本。

#### 6.3.3 JWT(可选备选,不取消)

无证书基础设施的场景(本地开发、某些 CI)可用标准 **Ed25519 JWT**(`Authorization: Bearer`,claims `iss=name/aud=gateway/iat/exp≤300s/jti`,EdDSA 验签)作为身份;`ROUTEGATE_CLIENT_AUTH=cert_or_jwt|jwt` 时启用。生产推荐证书。

#### 6.3.4 端用户上下文的可信转发

边缘网关对用户完成认证后,把用户标识放在头里传入。防伪造规则:**入站所有身份头一律清洗**,仅当「已验证的 caller ∈ `ROUTEGATE_TRUSTED_USER_CONTEXT_CALLERS` 设置(默认只有边缘网关身份)」时,才接受并重建 `X-Gateway-User-Id` 转发给上游;其他调用方携带的同名头一律剥离。上游因此只需要信任网关重建的头。

#### 6.3.5 网关 → 上游一跳

- 默认:私网明文 + 网络隔离(上游监听只在网关网可达)。
- 可选 TLS:网关出示自己的客户端证书(部署时挂载,`ROUTEGATE_UPSTREAM_TLS_CERT/KEY`);上游可钉网关证书验证——上游视角同样是「只维护与网关的一条信任关系」,与 hub-and-spoke 一致。

### 6.4 网络边界(ADR-0078 决策 4,部署指引)

- 数据面监听只加入「网关网」;上游服务监听只加入「网关网 + 自身业务网」,不暴露宿主端口;边缘网关经「网关网」访问 RouteGate。
- 部署文档提供参考网络拓扑;提供 `deploy/check-isolation.sh` 自检脚本,枚举「绕过网关直连上游监听」的可达性,用于验收「直连不可绕过」。
- 部署建议(v0.4.1,一句话级):默认单进程多监听形态下,数据面实例需同时接入网关网与管理网;**生产环境建议管理面独立部署(`ROUTEGATE_PROXY_ENABLED=false`),admin 端口永不与数据面同网暴露、绝不映射公网**。

### 6.5 访问决策审计(ADR-0078 决策 5)

每请求产生一条决策记录:`request_id、caller(证书主体)、route、operation(method+path)、matched_policy_id、policy revision、decision、upstream outcome、duration、bytes`。

- **Sink 插件化**:默认 `stdout`(JSON 行);`file`(轮转);`mysql`(可选,写 `access_log` 表)。
- MySQL sink 用**有界缓冲 + 批量写**(每 5s 或满 500 条 flush,多行 INSERT);缓冲满时**丢弃并计数**(`routegate_access_log_dropped_total`),绝不阻塞请求热路径。文档明示:审计强一致需求请用 file/stdout 采集进 Loki 等,MySQL sink 适合低流量场景。
- 不记录凭证、令牌、请求/响应体(ADR 红线)。需要抓取真实流量的排障场景走显式 opt-in 的调试日志(§6.6),不改变本红线。

### 6.6 调试日志(显式 opt-in 的敏感 artifact,v0.5 新增)

普通访问日志的红线(无凭证/无 body)不可破坏;排障需要看真实请求/响应时,走一个**完全独立、默认关闭**的调试通道:

- **开启方式**(v0.6 按所有者决策修订):`settings.debug_log` 行(JSON)是唯一配置源,**面板「设置」页维护**(`GET/PUT /admin/api/v1/settings/debug`,写仅 superadmin + 乐观并发 + 审计),实例在下一个同步 tick 应用——开关/路径/捕获上限都是运行时生效,不再有调试环境变量,也不再要求管理面 TLS。写路径是标准三合一事务(设置行 + `config_revision+1` + `admin_audit`);DB 不可达时沿用上一次的调试状态(fail-static)。原始的「必须 TLS + 重启生效」设计被所有者明确推翻:管理面按决策常驻明文 HTTP,安全边界交给部署层(管理网络/前置 TLS 代理),文档在 SECURITY.md 给出部署建议。
- **记录内容**:脱敏后的请求/响应头、请求/响应 Cookie、有界请求体/响应体(默认各 64 KiB,`*_MAX_BYTES` 可调;text/json/xml 存原文,其余 base64 并标注 content-type/encoding)、以及与普通记录一致的元数据(status、bytes、duration、decision)。超限标记 `truncated`,连接升级(WebSocket)或早期拒绝未消费请求体标记 `unavailable`。
- **IP 边界**(明确约定):不记录任何**显式网络地址字段**——`RemoteAddr`、`Forwarded`/`X-Forwarded-*`、`X-Real-IP`、`X-Client-IP`/`True-Client-IP` 一律排除,也不单独记录上游 IP;业务 body 或普通头值中自然出现的 IP 字符串不做全字节扫描(不现实且无意义)。头快照在 WriteHeader 时刻定格,之后设置的头(如部分 trailer)不含在内;长连接(流式/升级)在连接结束前可能没有最终调试记录——均文档明示。
- **凭证脱敏**:Authorization/Proxy-Authorization/API-key/token/signature/CSRF 系列头整头排除;Cookie/Set-Cookie 值中含 session/token/auth/secret/csrf/password/key 字样的一律 `<redacted>`。
- **Sink**:独立 JSON-lines 文件(不与普通 access log 共用路径与类型),异步有界队列,满载丢弃并计数,绝不阻塞业务请求;按日分段 + 2 MiB 滚动 + 保留期 janitor,机制与普通 file sink 一致。
- **权限**:列表接口(`GET /admin/api/v1/logs/debug`)仅返回元数据,所有管理员可见;payload 详情接口(`GET /admin/api/v1/logs/debug/{request_id}`)**仅 superadmin**,响应 `Cache-Control: no-store`,且每次查看写入 `admin_audit`(只记 request ID 与操作者,不记 payload)。服务端 RBAC 是最终防线,面板隐藏按钮只是体验层。

### 6.7 接口成功判定(可组合规则,v0.7 新增)

审计不能只靠状态码:业务网关普遍存在「HTTP 200 但业务失败」(如 `{"code":1}`)。每条路由可配置一个**成功判定规则**(`routes.success_rule`,JSON,默认空 = 不判定),数据面在响应代理完成后按规则给出 `success / failure` 结论,写入访问记录的 `outcome` 字段,面板访问日志页直接显示结果 Tag 并可按结果筛选:

- **条件**:来源 `status`(HTTP 状态码)或 `body`(响应体 JSON 字段,点分路径 `code`、`data.list`、数组下标 `items.0.name`);比较符 `in` / `not_in` + 值列表(单值 = 长度为 1 的列表);值统一字符串化比较(`0` ↔ `"0"`,容器按紧凑 JSON)。
- **组合**:每条规则自选 `all`(AND,默认)或 `any`(OR);1..16 个条件,值列表 1..64 项,规则 ≤ 4 KiB,加载/写入两侧均校验,坏规则丢弃并计数(路由本身照常服务)。
- **字段缺失语义**(所有者决策):引用的 body 字段缺失或响应体非 JSON 时,`in` 判失败、`not_in` 判通过——业务失败检测(`code in ["0"]`)与空 body 200 都符合直觉。
- **有界捕获**:仅配了规则(或业务字段提取/告警 body 条件)的路由在代理时额外捕获响应体前 8 KiB(独立于调试捕获,不缓冲全量、不破坏流式);SSE/长流式基于捕获前缀判断,文档明示。前缀在值中途被截断时,截断点之前已完整解析的顶层字段(业务信封 code/message 通常位于体首)保留参与判定与提取,被截断的字段按缺失处理;语法非法的响应体仍按非 JSON 处理(2026-11-10 修复:大响应截断曾把 200 + common.success 误判为 failure)。网关自身生成的 401/403/404 不参与判定(接口未被调到),上游失败(502/504)参与判定并按规则计为失败。
- **级别**:v1 挂在路由级(所有者决策,预留操作级扩展);规则随路由 CRUD 走标准三合一事务(校验 + revision + 审计),面板路由 drawer 内以「状态码/响应体字段 + 属于/不属于 + 值列表」的动态行编辑,并提供 **JSON 双向编辑**(表单 ⇄ JSON 互转 + 复制,便于把同一份规则批量导入多条路由;保存时后端仍统一校验)。
- **业务字段全局兜底**(所有者决策):`settings.biz_fields` 可配置全局的业务状态码/消息字段路径(`GET/PUT /admin/api/v1/settings/biz`,写仅 superadmin + 审计,实例下一 tick 生效)。路由自身配置的字段**按项优先**于全局;路由配置了但响应中缺失 → 该项为空,不跨字段猜测。

---

### 6.8 对客网关(APISIX)的两层集成(2026-11-07 决策)

南北向对客流量由 APISIX 承担(鉴权插件/限流/外部 API 形态),RouteGate 承担服务间 mTLS 身份与默认拒绝——两层互补,且「上游对客网关的调用也经过 RouteGate」本就是 §1 的能力范围。标准拓扑、三 prefix(admin/user/webhook)× 每 prefix 鉴权插件、APISIX upstream mTLS 配置与逐服务迁移手册见 **docs/DEPLOYMENT.md**;此处只记录架构决策:

- **身份模型**:APISIX 每个面对应一个 RouteGate 身份(`apisix-admin` / `apisix-user` / `apisix-webhook`),作为对应 upstream 的客户端证书钉在网关;两个 prefix 打同一服务时,RouteGate 视为同路由、不同 caller——审计可区分,策略可分化。
- **下游身份传递(v1 契约)**:RouteGate 剥离全部入向身份头,仅对 `ROUTEGATE_TRUSTED_USER_CONTEXT_CALLERS` 放行唯一的 `X-Gateway-User-Id`;APISIX 侧用命名前缀约定承载各面的鉴权主体(`admin:123` / `user:456` / `webhook:acme`)。按调用方可配置的**透传头白名单**列为 v1.x 候选特性,在真实需求出现前不实现;任意自定义头当前虽会透传,但不构成契约。
- **路径改写分层**:APISIX `proxy-rewrite` 负责外部命名 → 网关命名,RouteGate 的 `strip_prefix` 负责网关命名 → 服务命名,一层一跳不重复;RouteGate 的路由/策略/审计按改写后路径为准,外部路径留在 APISIX 日志(需要时以自定义头透传)。面上区分优先用**身份**而非路径改写,除非服务本身按路径路由;若使用改写,推荐机械约定而非逐条自由改写。
- **健康检查**:APISIX 不配对 RouteGate 的主动探针(探测 8080 需要客户端证书),实例级健康由部署平台打 `:8082/__ready`,多实例摘流由平台完成;超时/重试在 APISIX 按接口对齐网关错误信封,网关侧 fail-fast 不重试。

---

### 7.1 契约风格

- REST + 标准 HTTP 状态码(200/201/400/401/403/404/409/429/500),错误体 `{"error":{"code":"...","message":"..."}}`。
- 分页 `?page=&page_size=`(≤100),响应 `{"items":[...],"total":n}`。
- 全部写操作:同一事务内「写业务表 + `settings.config_revision+1` + 写 `admin_audit`」。
- **乐观并发**(v0.4.1 评审,用户确认 v1 就做):所有 PUT/DELETE 必须携带 `expected_revision`(客户端读取配置时的全局 config_revision);服务端在同一事务内校验,不一致 → `409 Conflict` + 返回当前最新配置,面板提示「配置已被他人修改,请刷新后重试」。v1.5 审核流的审批(approve)复用同一校验:申请时的基线 revision 已变则拒绝自动落库。**v1 豁免(2026-11-08 评审记录,只记录结论不改代码)**:users、告警渠道、告警规则的写操作不做 `expected_revision` 校验,按 last-write-wins 处理——三者均为低争用的 superadmin 资源,属有意豁免;routes/policies/settings 维持修订校验。
- 全部端点要求会话认证(§8.2);权限矩阵见 §8.1。
- **API 响应规范(v1,2026-11-08)**:所有非 2xx JSON 响应统一为 `{"error":{"code":"<domain>.<reason>","message":"<稳定英文句>","request_id"?}}`;状态码纪律 400 校验 / 401 未认证 / 403 禁止 / 404 不存在 / 409 冲突(修订冲突与唯一键冲突) / 500 基础设施(固定话术,细节仅入日志);错误响应带 `Cache-Control: no-store`;成功响应保持资源 JSON 不包信封;健康监听为探针契约(code-first),`/__metrics` 保持 Prometheus 文本,均豁免。修订冲突响应额外携带顶层 `config_revision`。

### 7.2 端点清单(v1)

```text
POST   /admin/api/v1/auth/login            → 会话 cookie(routegate-admin-session;HttpOnly;SameSite=Strict)
POST   /admin/api/v1/auth/logout
GET    /admin/api/v1/auth/me               → 当前用户/角色/是否需改密

GET/POST          /admin/api/v1/routes      PUT/DELETE /routes/{id}
GET/POST          /admin/api/v1/policies    PUT/DELETE /policies/{id}
GET               /admin/api/v1/policies?group_by=service
                                              → 矩阵视图数据源:caller × 被调服务聚合(v0.4.1)
GET               /admin/api/v1/policies/expiring   → 临期/通配规则专项列表
POST              /admin/api/v1/policies/test       → 模式试配:给 caller+method+path
                                                      返回命中规则与决策(面板调试利器)
GET/POST          /admin/api/v1/identities  PUT/DELETE /identities/{id}
POST              /identities/{id}/revoke
POST              /identities/validate      → 上传 PEM 预检(解析/有效期/指纹),不落库
POST              /identities/generate      → 服务端生成(备选路径):返回一次性私钥+证书;
                                             落库仅证书公体,审计只记事件与指纹(§6.3.2)

GET               /admin/api/v1/audit/admin         → 管理操作审计(筛选/分页)
GET               /admin/api/v1/audit/access        → 访问决策日志(MySQL sink 时可查)
GET               /admin/api/v1/logs/access         → 访问决策日志文件段读取(file sink)
GET               /admin/api/v1/logs/debug          → 调试日志元数据列表(全部管理员;§6.6)
GET               /admin/api/v1/logs/debug/{request_id}
                                                    → 调试日志 payload 详情(仅 superadmin,
                                                      no-store,查看行为入 admin_audit)
GET/PUT           /admin/api/v1/settings/debug      → 调试日志设置(settings.debug_log;
                                                      写仅 superadmin,运行时生效)

GET/POST          /admin/api/v1/users               PUT /users/{id}(启用/禁用/重置密码)
POST              /users/me/password                → 首登强制改密与自助改密

GET               /admin/api/v1/gateway/config      → 当前 DB 侧生效配置
GET               /admin/api/v1/gateway/snapshot    → 本实例当前内存快照(路由/策略/身份;原 /__routes 移入,会话认证)
GET               /admin/api/v1/gateway/instances   → 实例心跳(§3.5)

-- v1.5 预留(变更审核流,§8.5;v1 不实现)
-- POST /admin/api/v1/change-requests           普通用户提交路由/策略/身份变更申请
-- GET  /admin/api/v1/change-requests[/{id}]    列表/详情(diff)
-- POST /change-requests/{id}/approve|reject    超管审批;approve 原子落库+递增 revision
```

写入校验(前缀白名单、通配语法、targets JSON、证书 PEM 解析)在 API 层前置执行,错误信息面向面板表单展示;与 §3.2 加载器校验是同一套规则的两处调用,单测共用。

---

## 8. 管理账号体系

### 8.1 两级角色与权限矩阵

| 能力 | 超级管理员 `superadmin` | 普通用户 `user` |
| --- | --- | --- |
| 登录面板、查看配置/审计/实例状态 | ✓ | ✓ |
| 路由 / 策略 / 身份(证书)的增删改 | ✓ | **v1 否**(只读;已确认) |
| 用户管理(建号/禁用/重置密码) | ✓ | ✗ |
| v1.5:提交接口变更申请 | —(无需申请) | ✓(视需求实现) |
| v1.5:审核变更申请 | ✓ | ✗ |

角色固定两级,不做自定义权限表。

### 8.2 会话

- 服务端会话:32B 随机 token,仅存 **SHA-256 哈希**(`sessions` 表),cookie `routegate-admin-session`(HttpOnly、SameSite=Strict;纯 HTTP 部署决策下不设 Secure — 部署层保护)。
- 滑动过期 8h + 绝对过期 24h;登出/禁用即撤销;过期行由同步 Worker 顺带清理(§3.1)。
- 登录 Cookie 为**持久化**(`MaxAge` = 绝对过期 24h,HttpOnly、SameSite=Strict;管理面为明文 HTTP,未加 Secure——见部署边界):关闭浏览器不再丢会话,服务端滑动/绝对过期仍是唯一 enforcement(所有者决策 2026-11-08)。
- 不需要 Redis;与「MySQL 唯一依赖」一致。

### 8.3 登录加固

- 每 IP + 每用户名限流(内存令牌桶);失败计数指数锁定(`failed_login_count`/`locked_until`,5 次失败锁 15 分钟起);恒定时间比较;统一模糊错误文案;全部登录成败进 `admin_audit`。
- TOTP MFA:v1.5+(schema 可加列;密钥用可选的 `ROUTEGATE_MASTER_KEY` 加密落库)。

### 8.4 自举(Bootstrap)

- 首次启动且 `users` 为空:
  - 设了 `ROUTEGATE_ADMIN_BOOTSTRAP_USERNAME/PASSWORD` → 创建超级管理员,`must_change_password=1`;
  - 未设 → 生成随机密码,**一次性**打印到启动日志(文档显著警告);
  - 两者皆无且表为空 → Admin API 返回 503 `not_bootstrapped`,面板显示引导页。
- 自举后环境变量中的密码不再生效(防回写);普通用户一律由超管在面板创建。

### 8.5 v1.5:变更申请与审核(视需求实现,预留语义)

- 普通用户在面板对路由/策略/身份发起**变更申请**(create/update/delete 意图,携带 before/after 快照)。
- 申请进入 `pending`;超管在面板看到 **diff 预览**,批准则在同一事务内落库 + 递增 revision + 写审计;驳回需填意见。
- 届时新增 `change_requests` 表(status: draft/pending/approved/rejected/applied),不复用 v1 表。
- v1 的所有写操作都走「同一事务写业务表 + revision + 审计」封装(§7.1),v1.5 只是在这个封装外再包一层申请壳,**不需要返工 v1 代码结构**。

### 8.6 用户级临时 API 令牌(2026-11-09 增补:AI/机器客户端接入管理面)

- 动机:AI/机器客户端需要不共享浏览器会话、可独立吊销的管理面凭据。新增用户级**临时 Bearer 令牌**(`Authorization: Bearer rgt_<64 hex>`),一个用户可签发多枚;密文仅存 SHA-256 哈希,明文**仅创建时展示一次**,永不入日志;有效期强制(创建时设定 1..365 天),可随时吊销;创建/吊销写审计(`token.create`/`token.revoke`,resource_type `api_token`)。
- 授权语义(v1 明确):令牌**无独立权限范围**——它「作为其用户」认证,角色在每次请求时从 users 行实时解析(禁用用户/修改角色立即生效);更细粒度 scope 明确推迟。
- 生效范围:除 `auth/login`、`auth/logout`(保持会话语义)外,所有 admin 端点均接受 Bearer;请求携带 Bearer 头时不再回落会话 cookie。无效/过期/已吊销 → `401 admin.token_invalid`。
- 自助端点:`GET /admin/api/v1/auth/tokens`(本人令牌列表)、`POST /admin/api/v1/auth/tokens {name, expires_in_days}`(明文仅此一次返回)、`DELETE /admin/api/v1/auth/tokens/{id}`(吊销;他人 id 返回 404)。
- 发现端点(与令牌功能同批落地):`GET /admin/api/v1/meta/endpoints` 输出全部端点的机器可读目录(group/method/path/auth/summary/params/body_example),`GET /admin/api/v1/openapi.json` 由同一目录生成 OpenAPI 3.1(bearerAuth + cookieAuth 双 scheme);面板新增「开发者」页(我的凭证 / API 文档 / API 控制台)。

---

## 9. 管理面板(Web UI)

React 18 + TS + Vite + Ant Design 5(已确认);构建产物 `go:embed` 进二进制,由 admin listener 在 `/admin/` 提供(同源,无 CORS);i18n en + zh-CN。

**告警渠道**(v0.7 新增,§9.1):支持多个出站告警渠道,当前为飞书/Lark 机器人 webhook(同一 API,仅域名不同)。渠道是独立实体(`alert_channels` 表,迁移 0006):名称唯一、启用开关、平台、webhook token。安全契约——token 是凭证:列表仅掩码展示(`••••后四位`)、编辑留空即保留原值、审计行全程脱敏、日志永不记录;发送测试接口(`POST /alerts/channels/{id}/test`)响应不含 token。面板独立入口(超管)。API:`GET/POST /alerts/channels`、`PUT/DELETE /alerts/channels/{id}`、`POST /alerts/channels/{id}/test`;实例按同步周期加载渠道列表。

**告警规则**(v0.7 新增,§9.2):可组合触发器(`alert_rules` 表,迁移 0007)——作用范围(全部路由/指定路由)、一个告警渠道、条件语法与路由成功判定一致并新增 `outcome` 来源(判定成功/失败,取自路由自身的成功判定;路由未配置判定时该条件按缺失语义处理),以及告警专有的 `biz_code`/`biz_message` 来源(2026-11-10 增强,迁移 0013 同步落库:取路由业务字段提取——路由级路径按项优先、全局兜底,§6.7;提取为空按缺失语义处理,正向不命中、not_* 空真)。组合 all(默认)/any。**暂不做降噪与限频**(所有者决策):每条命中即触发。派发经**有界异步队列**(满载丢弃计数,绝不阻塞流量),webhook 发送(尊重 HTTPS_PROXY、5s 超时)后**告警记录**写入 `alert_events`(命中条件、HTTP 状态、判定、业务码与业务消息、sent/send_error——飞书对无效 token 也返回 200,送达判定解析响应体 code);飞书/Lark 告警卡片同时显示触发请求的 API 路径。面板独立「告警规则」页含告警记录表;API:`GET/POST /alerts/rules`、`PUT/DELETE /alerts/rules/{id}`、`GET /alerts/events`。

**多层嵌套 + JSON 文本**(v0.7 增强):匹配文档升级为**递归节点树**——节点是叶子条件(同上语法)或组(`mode: all|any` + `children`,组可嵌套组,深度 ≤3、节点 ≤32);旧平铺 `{mode, conditions}` 文档完全兼容(读取时自动包装)。面板规则编辑器提供 表单(JSON 平铺)/ JSON(完整嵌套文档,含复制/粘贴)双视图,嵌套规则在 JSON 视图编辑;后端保存时统一校验。

| 页面 | 内容 |
| --- | --- |
| 登录 / 首登改密 | — |
| Dashboard | config_revision、同步健康、路由/策略/身份数、近期 deny Top、实例传播状态(N/M) |
| 路由 | 列表 + 表单(前缀校验、targets 编辑、strip、methods、启用);展示「该路由受哪些策略保护」反查 |
| 授权策略 | 列表 + 表单(模式编辑器:即时校验通配语法);**矩阵视图(caller × 被调服务聚合)**——一个服务的多条路由聚合为一列、格内按路由分组、点击展开明细,「A 能访问 B 的什么」一眼可答(v0.4.1:策略锚在 route 上,聚合视角在视图层补齐服务维度可读性);`/**`/临期高亮;**模式试配器**(输入 caller+method+path → 即时显示命中与决策,§7.2) |
| 身份与证书 | 列表;三种接入:上传 PEM / **面板生成**(浏览器端 WebCrypto 生成,私钥一次性下载,§6.3.2)/ JWT 公钥录入;轮换(同 name 并存)/ 吊销;展示引用该 caller 的策略数 |
| 访问审计 / 管理审计 | 筛选查询(caller、路由、decision、时间窗) |
| 用户管理(仅超管可见) | 建号、角色、禁用、重置密码 |
| 变更申请(v1.5,普通用户入口) | 提交/我的申请;超管侧 diff 审核页(规划) |
| 系统状态 | 实例心跳表、版本、同步延迟 |

面板不做任何「绕过 API 的直写」;所有变更走 Admin API,天然获得审计与 revision 递增。

---

## 10. 可观测性

- 端点位置(v0.4.1):`/__health`(liveness)、`/__ready`(DB ping + 最近同步成功 + stale 判定)、`/__status`(revision、实例 ID、版本、身份验证模式、证书材料计数)**全部且仅在健康监听 `ROUTEGATE_HEALTH_LISTEN`(默认 `:8082`)上提供**,语义沿用现实现(200/503 标准状态码)。`/__routes` 调试快照端点从数据面移除,改为 Admin API `GET /admin/api/v1/gateway/snapshot`(会话认证 + 审计可见,比原裸端点更安全)。
- **Prometheus 指标**(v1 落地,2026-11-08):`GET /__metrics` 挂在健康监听上(不单开 `:9091`,少一个端口与攻击面),Prometheus 文本格式、零新依赖; scrape 不触碰 DB。覆盖进程内可直接读到的运行状态:build info/实例、uptime、Go runtime、快照状态(loaded/revision/routes/identities/loaded_at)、同步 ready 与最近成功时间、告警规则/渠道加载与启用数、告警队列丢弃计数、debug 开关与丢弃计数、访问日志丢弃计数(`DroppedReporter`)。暴露的只有计数器与状态值,不含任何请求数据或凭证。按 route/decision 的请求计数与延迟直方图需要数据面写入注册表,列为后续候补(不阻塞 v0.1.0)。
- 结构化日志:JSON,字段与访问决策对齐(request_id / caller / route / decision),便于 Loki 检索。
- 追踪:透传 `traceparent`(继承现实现);自身 span 导出为 v2 可选。

---

## 11. 安全设计要点

1. **默认安全、无旁路**:身份验证 + 策略评估永远执行(默认拒绝);不存在免鉴权模式;`cert_or_jwt`/`jwt` 仅是凭据形式差异。
2. 凭据红线:DB 中只有证书公体/公钥与密码哈希;网关不存任何服务私钥;面板生成路径的私钥只在交付瞬间出现(§6.3.2);审计与日志不落凭证/令牌/报文。
3. 管理面隔离:admin listener 默认绑管理网络;文档给出「禁止与数据面同域公网暴露」的部署红线。
4. 请求头卫生:入站身份/代理头一律清洗;端用户上下文仅从可信边缘身份重建(§6.3.4),防止身份走私。
5. 路径归一化模糊测试(§6.2)防通配绕过;通配语法严格化(`**` 仅末段)防歧义。
6. 证书校验完备:指纹精确匹配 + 有效期 + 吊销 + SAN↔name 一致性(§6.3.2);吊销下一同步周期全网生效。
7. 依赖白名单(§2.3)最小化供应链面;CI 跑 `govulncheck` + 前端 `npm audit`。
8. `SECURITY.md` 私密漏洞上报渠道;发布物料附带 SBOM(`syft` 生成)。
9. **DB 访问姿态(2026-11-05 用户决策:单 DSN,不分离)**:评审曾建议数据面/管理面双 DSN 最小权限分离,为保持部署最简被否——数据面实例因此持有全库写权限,该风险已知并接受。缓解要求:MySQL 监听仅对网关实例所在网络可达(网络 ACL),DB 不暴露给任何业务服务;文档披露此姿态,未来如需收紧可在不破坏兼容的前提下增加可选的 `ROUTEGATE_ADMIN_DB_DSN`。

---

## 12. 代码仓库结构

```text
routegate/                        # github.com/BadmintonCode/routegate-gateway
├── cmd/routegate/main.go         # serve / migrate / version
├── internal/
│   ├── config/                   # 环境变量解析、默认值、校验
│   ├── store/                    # MySQL:迁移、仓储、revision 事务封装
│   ├── snapshot/                 # 快照模型、加载器、原子持有者(含证书材料预解析)
│   ├── proxy/                    # 匹配、转发、LB、被动健康、头清洗重建
│   ├── authz/                    # 通配策略评估器(段树)、模式试配
│   ├── identity/                 # mTLS 证书验证/指纹匹配、JWT 备选验签
│   ├── accesslog/                # sink 接口 + stdout/file/mysql 批量实现
│   ├── adminapi/                 # REST 处理器、会话、角色、自举、证书生成(备选)
│   └── web/                      # embed 的面板产物
├── web/admin/                    # React 面板源码(含 WebCrypto+PKI.js 证书生成)
├── migrations/                   # *.sql(嵌入二进制)
├── deploy/                       # Dockerfile、compose quickstart、网络拓扑示例、自检脚本
├── docs/                         # 架构、配置参考、部署、证书运维手册、威胁模型
├── scripts/                      # 开发与自检脚本
├── .github/workflows/            # CI/CD
├── LICENSE(Apache-2.0)
├── README.md / CONTRIBUTING.md / CODE_OF_CONDUCT.md / SECURITY.md / CHANGELOG.md
└── ADR-0001-*...                 # 新仓库自己的决策记录(继承本设计为 ADR-0001)
```

---

## 13. 环境变量参考(汇总)

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `ROUTEGATE_DB_DSN` | 必填 | MySQL DSN |
| `ROUTEGATE_CONFIG_SYNC_INTERVAL` | `60s` | 配置同步间隔(用户核心诉求) |
| `ROUTEGATE_LISTEN` | `:8080` | 数据面监听 |
| `ROUTEGATE_PROXY_ENABLED` | `true` | 关闭后成为纯管理面实例 |
| `ROUTEGATE_ADMIN_LISTEN` | `:8081` | 管理面监听 |
| `ROUTEGATE_ADMIN_ENABLED` | `true` | 数据面扩容实例可关 |
| `ROUTEGATE_HEALTH_LISTEN` | `:8082` | 健康检查监听(明文、无证书、仅内部网;§2.2) |
| `ROUTEGATE_HEALTH_ENABLED` | `true` | 关闭需自行另配探针(文档警示) |
| `ROUTEGATE_CLIENT_AUTH` | `cert` | `cert` / `cert_or_jwt` / `jwt`(均为强制鉴权,§5.1);本 release 仅实现 `cert`,`cert_or_jwt`/`jwt` 在加载时被拒绝 |
| `ROUTEGATE_SERVER_TLS_CERT/KEY` | 必填 | 数据面服务端证书 |
| `ROUTEGATE_TRUSTED_USER_CONTEXT_CALLERS` | 空 | 允许转发端用户上下文的 caller 名单(§6.3.4) |
| `ROUTEGATE_UPSTREAM_TLS_CERT/KEY` | 空 | 网关→上游一跳的客户端证书(可选)(规划/M2,未实现) |
| `ROUTEGATE_UPSTREAM_KEEPALIVE` | `true` | 上游连接复用(v0.4.1 起默认开启;`false` 回退「每请求新建连接+重解析」旧行为) |
| `ROUTEGATE_UPSTREAM_DNS_TTL` | `5s` | 上游 DNS 缓存 TTL,即副本扩缩容后重平衡的时延上界 |
| `ROUTEGATE_READY_STALE_AFTER` | 3×interval | 就绪降级阈值 |
| `ROUTEGATE_DRAIN_TIMEOUT` | `20s` | SIGTERM 排水宽限(§14) |
| `ROUTEGATE_BOOT_MAX_WAIT` | `0`(不超时) | 启动连不上 DB 的最长等待 |
| `ROUTEGATE_INSTANCE_ID` | hostname | 实例标识 |
| `ROUTEGATE_ACCESS_LOG_SINK` | `stdout` | `stdout` / `file`(按日分段+大小滚动+保留期 janitor)/ `mysql`(M2) |
| `ROUTEGATE_ACCESS_LOG_MYSQL_FLUSH` | `5s` / 500 行 | 批量写参数(规划/M2,未实现) |
| `ROUTEGATE_ACCESS_LOG_MAX_MB` | `2` | file sink 单段文件上限,超过滚动(新记录仍写完自己所在的段) |
| `ROUTEGATE_ACCESS_LOG_RETAIN_DAYS` | `7` | file sink 段保留天数,超期由每小时 janitor 按文件名日期删除 |
| `ROUTEGATE_ADMIN_BOOTSTRAP_USERNAME/PASSWORD` | 空 | 首次自举(仅超管) |
| `ROUTEGATE_MASTER_KEY` | 空 | MFA 密钥加密用(v1.5,可选)(规划/M2,未实现) |
| `ROUTEGATE_ADMIN_TLS_CERT/KEY` | — | **已移除**(所有者决策):管理面常驻明文 HTTP;生产用管理网络隔离或前置 TLS 代理 |

> DSN 约定:`store.Open` 会为连接会话固定 `time_zone='+00:00'`(DSN 已显式指定 `time_zone` 时不覆盖;显式给了非 UTC `loc` 则告警并保留)。原因:驱动按 UTC 墙钟写 DATETIME,而 `NOW()` 按会话时区求值——服务器时区非 UTC 时,管理会话 GC(`expires_at < NOW()`)会在一个同步周期内删掉刚创建的会话,策略过期判断同样偏移。

调试日志(§6.6)没有环境变量:全部配置存于 `settings.debug_log`(JSON,默认关闭),面板「设置」页维护,写仅 superadmin + 乐观并发 + 审计,实例下一同步 tick 运行时生效。默认值:单段 2 MiB、保留 7 天、请求/响应体捕获各 64 KiB、头 32 KiB、队列 1024。

---

## 14. 高可用与故障语义(ADR-0078 决策 6)

**网关是全流量的关键路径**(能力范围 1),HA 是硬要求:

- **无状态横向扩展**:数据面 N 实例(≥2、跨宿主)+ LB;实例间无协调(快照各自从 MySQL 拉,revision 误差 ≤ 一个同步周期,心跳面板可见)。
- LB/编排系统健康检查打**健康监听**(`ROUTEGATE_HEALTH_LISTEN`,默认 `:8082`)的 `/__ready`(标准 200/503,与 APISIX upstream check、Docker HEALTHCHECK、K8s probe 兼容;不带证书——数据面 `:8080` 的证书强制与探针不兼容,v0.4.1 评审修正)。
- 失败行为矩阵:上游超时/不可达 → 明确错误信封;策略与身份评估无外部依赖(纯内存+证书材料常驻)→ **DB 宕机不影响存量流量**,只影响配置新鲜度;管理面宕机不影响数据面。
- 容量:所有南北+东西流量都过网关,单实例为协程转发模型,TLS 终止 + 内存匹配的开销可控;起步压测基线建议 ≥ 5k QPS/实例;调用方启用连接复用摊薄握手成本。**M1 压测门槛**(v0.4.1):≥5k QPS/实例且 P99 达标 + 副本扩容后 ≤ DNS TTL(5s)内重平衡生效;不达标则回退 `ROUTEGATE_UPSTREAM_KEEPALIVE=false` 默认并记录 ADR。
- **fail-closed**:策略缺失/快照为空/证书验证失败 → 一律拒绝;唯一非 fail-closed 点是 DB 宕机时沿用旧快照(文档明示「fail-static」,配置陈旧窗口 ≤ stale 阈值 + LB 摘除)。
- **MySQL 可用性是网关跨重启生存能力的上界**(v0.4.1 评审,定位为**强烈建议**而非准入门槛):运行中实例靠 fail-static 存活,但 DB 宕机期间的任何实例重启/扩容,都会以「空快照 = 全拒绝」加入集群,而网关承载全部服务间流量。生产环境强烈建议:MySQL 高可用(托管服务或主从 + 自动切换)、`ROUTEGATE_DB_DSN` 指向**稳定端点**(LB 虚拟 IP / DNS,而非单主机)、其可用性等级不低于网关自身目标;部署文档完整披露该风险链,由部署方按自身风险选择(开源项目不设硬性门槛,quickstart 单实例 MySQL 可用)。
- **停机语义**(v0.4.1 用户决策:**v1 不做排水**;**2026-11-08 修订:排水已实现**,同轮 owner 批准):收到 SIGTERM → `/__ready` **立即 503** 加速 LB 摘流(liveness `/__health` 保持 200,排水不触发重启)→ 停收新连接 → 在途请求排空(`ROUTEGATE_DRAIN_TIMEOUT`,默认 `20s`,超时立即关闭)→ 数据面之后依序关闭管理面与健康监听 → 冲刷日志退出。**调用方仍必须以重试 + `Idempotency-Key` 自保**(宽限内的长连接/SSE 会随超时被切断,滚动发布仍有一小波脉冲)。告警队列/调试 sink 在退出时按 v1 语义不排空(在途触发丢弃,计数器可见)。

---

## 15. 开源规划

### 15.1 里程碑

| 里程碑 | 内容 | 粗略规模 |
| --- | --- | --- |
| **M0 立项** | 建仓 `BadmintonCode/routegate-gateway`、LICENSE、CI 骨架、本设计入库为 ADR-0001 | 1 天 |
| **M1 数据面 MVP** | schema+迁移、同步 Worker(60s/env)、路由+转发+轮询+健康、requestId、头清洗、compose quickstart、单测+testcontainers 集成测试 | 1–2 周 |
| **M2 身份与策略** | mTLS 服务端+客户端证书验证(指纹钉证书)、JWT 备选、通配策略评估器(段树)、路径归一化+模糊测试、访问日志 sinks、心跳/__status、证书生成辅助 | 2 周 |
| **M3 管理面** | Admin API 全量、两级角色+会话+自举、管理审计、面板全部页面(含矩阵视图/模式试配器/证书管理与浏览器端生成)、embed 构建 | 2–3 周 |
| **M4 开源就绪** | README/文档站、demo 场景(边缘网关模拟调用方 + 两个服务 + allow/deny 演示)、安全自查、goreleaser、**v0.1.0 公开发布** | 1 周 |
| **v1.5** | 变更申请-审核流(视需求)、TOTP MFA、内部 CA 模式、上游一跳 TLS 强化、每实例本地紧急吊销指令 | 规划 |
| **v2** | OIDC/LDAP 企业登录、config hint 准实时、gRPC 路由 | 规划 |

### 15.2 许可与社区

- 许可:**Apache-2.0**(含专利授权,企业采用友好);CONTRIBUTING 走 DCO(`Signed-off-by`),不设 CLA。
- 仓库:`github.com/BadmintonCode/routegate-gateway`(当前 org,已确认);对外开源时可平移/transferred 到独立 org,不影响 module path 之外的结构。

### 15.3 CI/CD 与发布

- GitHub Actions:lint(golangci-lint + eslint)、测试(Go 单测 + testcontainers-MySQL 集成)、`govulncheck`、面板构建、Docker 镜像构建、goreleaser 出二进制 + 镜像(ghcr)(M4 落地现状:ci.yml 仅含 gofmt/vet/build/test -race 与面板构建;lint/govulncheck/集成测试为后续项)。
  **落地修订(2026-11-08)**:CI 提供 Go/面板构建与测试(`ci.yml`);**镜像统一走发布控制台**构建(私有 registry 源,部署轮 owner 决策,ghcr 未启用);tag `v*` 触发 `release.yml` → goreleaser 出二进制 + checksums + SBOM 到 GitHub Release(`.goreleaser.yaml`,面板 dist 先行构建;镜像内版本经 `ROUTEGATE_VERSION` build-arg 注入)。
- 语义化版本;CHANGELOG Keep-a-Changelog 格式;v0.x 允许 breaking(v1.0 后遵守)。
- 发布物料:二进制、Docker 镜像、SBOM、checksums 签名(cosign 可选)。

### 15.4 文档与定位

- README 三段式:是什么 / 60 秒 quickstart(compose 起 MySQL + 网关 + 两个 demo 服务,面板里生成两张演示证书、试配一条策略即可看到 allow/deny)/ 与 APISIX、Kong、Envoy 的定位对比。
- 对比定位(诚实表述):比 APISIX/Kong 轻(无 etcd/控制面集群,DB 即控制面)、东西向默认拒绝内建、**服务间 mTLS 采用 hub-and-spoke(每服务只维护与网关一条信任关系,面板可生成证书)**、面板开箱即用;不做插件市场与网格。
- docs/:架构(继承本文)、配置参考(§13 全表)、部署与网络隔离指引(§6.4)、**证书运维手册(生成/上传/轮换/吊销,§6.3)**、威胁模型、迁移指南。

---

## 16. 组织采用(独立后续任务,不由本设计授权)

本设计是通用开源项目;任何组织如采用,按独立实施计划**单独立项**:一次性导入现有路由表(每行 prefix → route + targets JSON)、为各服务生成/登记证书、边缘网关注册为可信调用方 → 逐服务放流量。全程遵守所在组织的变更治理流程。

---

## 17. 验收口径(设计层面的 Done)

- [ ] 改一条路由/策略/证书,`≤ SYNC_INTERVAL` 内全实例生效,心跳页可见传播状态。
- [ ] 无 allow 规则的调用被拒;deny 优先于 allow;默认拒绝成立;不存在任何免鉴权旁路。
- [ ] 证书链路:无效/过期/被吊销证书、合法证书但 SAN 冒用他人名 → 全部拒绝;轮换期间新旧证书并存可用。
- [ ] 面板生成:浏览器端生成 → 私钥一次性下载 → 公体落库 → 下个周期可用;服务端/DB/审计日志中无私钥痕迹。
- [ ] 通配语义:`/v1/users/*` 不匹配零段与多段;`/v1/*/detail` 命中中段;`/**` 强制带过期;归一化后 `/v1/../v1/users/123` 无法绕过。
- [ ] 伪造 `X-Gateway-User-Id` 的非可信调用方,其头被剥离;可信边缘网关的用户上下文正常透传。
- [ ] DB 宕机:存量流量不受影响,`/__ready` 按 stale 转 503,恢复后自动追平。
- [ ] 普通用户登录后只能只读;超管可完成完整闭环:建路由/策略 → 试配器确认 → 看到流量放行 → 改 deny → 看到拒绝 → 审计里查到这两条管理操作与对应访问决策。
- [ ] quickstart 一条命令拉起,自检脚本证明「绕过网关直连上游被网络隔离阻断」。
- [ ] SSE/流式响应经网关透传无攒批(FlushInterval 生效);WebSocket Upgrade 透传(实验性标注)。

---

## 18. 决策记录(2026-11-05 与用户对齐,全部已定)

| # | 决策项 | 结论 |
| --- | --- | --- |
| 1 | 项目名 | **RouteGate**,仓库 `github.com/BadmintonCode/routegate-gateway` |
| 2 | 管理面板技术栈 | React 18 + TypeScript + Vite + Ant Design 5 |
| 3 | 无鉴权模式 | **删除**;身份验证 + 策略永远执行(默认拒绝) |
| 4 | 仓库宿主 | github.com/BadmintonCode(个人开源仓库) |
| 5 | 普通用户权限 | v1 只读;变更申请-审核流 v1.5 视需求实现 |
| 6 | 证书方式 | 逐服务钉证书;**支持面板生成**(浏览器端生成为主,私钥不经服务端);内部 CA 移 v1.5 |
| 7 | 数据库 / 同步 | MySQL 唯一控制面;Worker 周期加载,`ROUTEGATE_CONFIG_SYNC_INTERVAL` 默认 60s |
| 8 | 角色体系 | 超级管理员 / 普通用户两级,固定不扩展 |
| 9 | DB 账号模型(v0.4.1 评审) | **单 DSN 不分离**(双 DSN 提案被否);数据面持全权为已知接受风险,缓解=MySQL 仅对网关网络可达 |
| 10 | 健康检查监听(v0.4.1 评审 P0) | 独立明文监听 `ROUTEGATE_HEALTH_LISTEN`(默认 `:8082`,可配置可关闭);`:8080` 无健康端点 |
| 11 | 吊销缓解(v0.4.1 评审 P1) | 生成证书默认 90 天;预案明示吊销前置为 MySQL 可用;v1.5 预留本地紧急指令 |
| 12 | MySQL HA(v0.4.1 评审 P1) | **强烈建议而非硬门槛**(用户决策);风险链完整披露于 §14 与部署文档 |
| 13 | 优雅停机(v0.4.1 评审 P1) | **v1 不做**(用户决策):SIGTERM 即退出;调用方以重试+幂等键自保,文档明示;排水列为候补 |
