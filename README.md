# 城市体育馆 · 羽毛球场地预约 / 灯控 / 计费 / 爽约处罚服务

Go + PostgreSQL + Redis 实现的后端服务。覆盖预约下单、支付与超时释放、闸机放行、灯控预热/开灯/关灯、
前台签到、延时与拼场超员计费、退订规则、爽约/迟到/场馆原因自动裁决、场地维护锁定与**临时换场**
（用户可换场/顺延/退款，高价差价场馆承担，闸机灯控权限同步新场地，维护完工需前台验收才重新开放）、
临时加场、会员权益抵扣与客服争议处理。所有状态变化都写入统一事件日志，客服可查看「预约 + 支付 + 闸机 +
灯控 + 前台 + 维护 + 换场」完整时间线，而不必只按订单表猜测。

- 语言/框架：Go（标准库 `net/http`，无 Web 框架）、PostgreSQL 15、Redis 7
- 数据库迁移：内嵌 SQL（`internal/store/migrations/0001_init.sql`），启动自动执行
- 接口文档：OpenAPI 3.0 + Swagger UI（静态资源已用 `go:embed` 打进二进制，**离线可用**）
- 依赖已 `go mod vendor`，Docker 构建无需联网拉 Go 模块

---

## 1. 一键启动（Docker Compose）

```bash
docker compose up -d --build
```

启动后：

| 入口 | 地址 |
| --- | --- |
| Swagger UI（推荐从这里调试） | http://localhost:8080/swagger/ |
| 健康检查 | http://localhost:8080/api/v1/health |
| API 前缀 | http://localhost:8080/api/v1 |

服务首次启动会自动建表并写入种子数据（1 个场馆、10 片场地（8 片普通 + 2 片 VIP 高价场地）、
3 个会员、3 个员工、高峰窗口与退订规则）。

停止并清空数据：

```bash
docker compose down -v
```

### 验证模式（可重复的端到端脚本）

端到端脚本需要自行推进「虚拟时间」并显式触发后台任务，因此使用关闭自动任务的覆盖配置：

```bash
docker compose -f docker-compose.yml -f docker-compose.verify.yml up -d --build
BASE=http://localhost:8080 bash scripts/e2e_test.sh
```

> compose 默认配置开启了 `ALLOW_TEST_CLOCK`（演示用虚拟时钟，可通过 `/internal/clock` 推进时间）。
> 生产部署请将该变量设为 `false`，此时 `/internal/clock` 会返回 403，系统始终使用真实时间。

---

## 2. 测试账号（种子数据，密码均已 bcrypt 入库）

### 员工

| 角色 | 用户名 | 密码 | 权限 |
| --- | --- | --- | --- |
| 场馆运营 | `operator` | `operator123` | 维护锁定、临时加场、退订/爽约规则、代客退订 |
| 前台 | `front` | `front123` | 签到、核对人数/超员、补偿时长、开始/结束场次 |
| 客服 | `cs` | `cs123` | 查看时间线、裁决建议、争议单处理（退费/补偿/撤销爽约/驳回） |

### 会员（同一场馆）

| 会员 | 手机号 | 密码 | 权益（种子策略） |
| --- | --- | --- | --- |
| 普通会员 张伟 | `13800000001` | `123456` | 原价，可提前 24h；高峰开场前 24h 内才开放 |
| 银卡会员 李娜 | `13800000002` | `123456` | 9 折，每月 2 小时免费，可提前 72h |
| 金卡会员 王强 | `13800000003` | `123456` | 8 折，每月 5 小时免费，可提前 168h |

可通过 `POST /api/v1/auth/register` 注册新会员（默认 normal）。

登录示例：

```bash
# 会员
curl -s -X POST http://localhost:8080/api/v1/auth/login/member \
  -H 'Content-Type: application/json' \
  -d '{"phone":"13800000002","password":"123456"}'
# 员工
curl -s -X POST http://localhost:8080/api/v1/auth/login/staff \
  -H 'Content-Type: application/json' \
  -d '{"username":"cs","password":"cs123"}'
```

除闸机/灯控设备端与注册登录外，所有接口都需要 `Authorization: Bearer <token>`。

---

## 3. 本地开发（不使用 Docker）

前置：Go 1.19+、PostgreSQL、Redis。依赖已 vendor，直接 `go build -mod=vendor`。

```bash
# 1) 准备数据库与 Redis（示例）
createdb badminton
export DATABASE_URL='postgres://user@127.0.0.1:5432/badminton?sslmode=disable'
export REDIS_ADDR=127.0.0.1:6379

# 2) 运行（自动迁移 + 自动种子）
go run -mod=vendor ./cmd/server
# Swagger: http://localhost:8080/swagger/
```

若当前机器已有 PostgreSQL 但无系统库管理权限（如 CI/沙箱），可用用户态集群一键完成
「初始化 PG(5433) + 复用本机 Redis + 起服务 + 跑全部端到端断言」：

```bash
make e2e          # 等价于 bash scripts/local-e2e.sh
```

常用命令：`make build` / `make vet` / `make fmt` / `make docker-up` / `make docker-verify`。

---

## 4. 核心设计

### 4.1 预约状态机

```
held ──支付成功──▶ paid ──到达预热点(开场前10min)──▶ warming ──开场/人工开灯──▶ in_use ──结束结算──▶ completed
  │                 │                                  │
  │支付超时          │退订                              │退订（已预热，关灯留痕）
  ▼                 ▼                                  ▼
expired         cancelled                          cancelled
held 遇维护锁定 ─▶ expired；in_use 遇维护锁定 ─▶ venue_fault（自动全额退款 + 开争议单）
paid/warming 遇维护锁定 ─▶ 生成换场方案（relocation_offers，订单保持 paid 待用户确认）：
   用户换场/顺延 ─▶ 旧单 relocated（维护原因写入旧单）+ 新订单继承价格与已支付金额
   用户退款      ─▶ venue_fault（全额退款 + 开争议单）
   用户不响应    ─▶ 开场后 20min 裁决兜底：venue_fault 自动全额退款，方案自动 expired
paid/warming 开场后 20min 仍无到场证据 ─▶ no_show（扣爽约次数 + 自动开争议单）
证据不足（有退款申请/灯开了未上场） ─▶ disputed（客服人工裁决）
```

- **支付超时**：后台任务（`/internal/jobs/expire-unpaid`，进程内每 20s）释放 `held` 订单占用的场地与权益预占。
- **灯控预热是独立状态 `warming`**：一旦进入预热，支付超时任务**绝不会回收**该订单，避免"已开灯等客人却被系统取消"。
- 所有状态更新都在数据库事务内 `SELECT ... FOR UPDATE` 完成，并伴随 `event_log` 留痕。

### 4.2 并发预约防重（双保险）

1. **Redis 临界锁**：`SET NX PX`（court + 精确起止时间维度，Lua 脚本按 token 释放），快速拒绝同刻并发请求；
2. **PostgreSQL GiST 排他约束**：
   ```sql
   alter table bookings add constraint bookings_court_exclusive
     exclude using gist (court_id with =, tstzrange(start_time,end_time,'[)') with &&)
     where (status in ('held','paid','warming','in_use'));
   ```
   连「时间段重叠（非完全相同时段）」与「Redis 恰好不可用/锁过期」的情况也由数据库兜底。
3. 客户端可传 `idempotency_key`，重试安全返回同一订单。

### 4.3 计费规则（金额单位：分）

- 场地费按**分钟分段**：每分钟判断是否落在高峰窗口（工作日 18:00–21:00；周末 09:00–12:00、14:00–17:00），
  高峰费率 = 基础价 ×1.6667（60 元→100 元/小时），平/高峰分钟分别汇总折算，整小时精确无取整误差。
- 会员折扣只作用于场地费（银卡 9 折、金卡 8 折）。
- **权益抵扣**：月度免费时长 + 前台/客服发放的补偿券；下单即**预占**（reserved），支付转核销，超时/退订释放，
  避免两笔待支付订单重复使用同一份免费时长；下单时可用 `use_benefit:false` 不使用权益。
- **延时费**：超过「预约结束 + 补偿时长」的部分按 15 分钟向上取整，费率取延时时刻（可能命中高峰）×1.5。
- **拼场超员费**：实际人数 − 场地容量，每人 20 元；前台可整笔减免。
- 租拍 10 元/支、球 15 元/筒；退订退款按平峰/高峰分档规则计算。

### 4.4 闸机与灯控放行

- 闸机扫码（`POST /api/v1/devices/gate/scan`）结合**支付状态 + 入场时间窗口（开场前 20min ~ 开场后宽限）**：
  - 未支付/已取消/已爽约 → `denied`；
  - 提前太早 → `denied`；
  - 已到场但场地维护中，或**前一场延时未结束** → `waiting`（记录 `gate_waiting_at`，在场边等待）；
  - 正常 → `allowed`。
- 灯控指令 `warmup/on/off` 带来源（`scheduler/device/front/operator/system`），全部写 `light_records`。
- 因前一场延时导致晚开时，开灯自动补偿等额可打时长（`comp_bonus_minutes`，结算时优先抵扣延时费）。
- 调度器在「前一场仍在延时占用」时**不会**自动给后一场开灯（保留 warming 交前台处理），但人工可强制开灯处理灰度。

### 4.5 爽约 / 迟到 / 场馆原因裁决（证据优先级）

开场后 20 分钟自动裁决（也可用 `GET /bookings/{id}/verdict` 预览，不落库）。证据来自独立记录表：

| 证据 | 来源表 |
| --- | --- |
| 闸机放行 / 场边等待 | `gate_records`（result=allowed/waiting） |
| 前台签到 | `front_records`（action=checkin） |
| **人工**开灯 | `light_records`（action=on 且 source∈device/front/operator） |
| 退款申请 | `refund_requests`（pending） |
| 场地维护 | `maintenance_windows` |
| 前一场延时 | 前序 in_use 订单未结束 |

判定优先级：**维护 → 前一场延时未结束（且未开打）→ 人工开灯且到场（按时/迟到）→ 灯开了但无到场（ambiguous）
→ 有退款申请但无证据（ambiguous）→ 爽约**。

- 调度器**自动开灯不构成到场证据**，因此"灯开了但用户没上场"不会被误判为正常，也不会掩盖爽约。
- 判爽约：会员 `no_show_count +1`，自动开争议单；判场馆原因：自动全额退款并开争议单；
  ambiguous：进 `disputed` 等客服。客服可全额退/部分退/发补偿时长券/撤销爽约并返还次数/驳回。

### 4.6 现场灰度场景的处理与留痕

| 现场情况 | 接口 | 系统行为 |
| --- | --- | --- |
| 用户在场边等、前一场延时未结束 | 闸机扫码 | `waiting` + `gate_waiting_at`，晚开自动补等额时长 |
| 灯开了但人没上场 | 设备 `light on` 后无闸机/前台记录 | 裁决 `ambiguous` 进争议单，闲置超时自动关灯 |
| 会员带朋友拼场超员 | `/participants` + `/headcount` | 按超员人数计费，`/waive-overlimit` 可减免 |
| 前台临时补偿 | `/compensation` | `scope=current` 延长本场；`scope=future` 发补偿券 |
| 场地临修 | `/operator/maintenances` | 锁定排期；held 释放、in_use 全退开单、paid/warming 生成换场方案 |
| 临时加场 | `/operator/extra-slots` | 营业时段外开放，排期接口可见可约 |

每个动作都同时写对应业务表（gate/light/front/payments/maintenance…）与统一 `event_log`（含 source、actor、payload），
`GET /bookings/{id}/timeline` 一次返回客服裁决所需的全部证据。

### 4.7 场地维护临时换场与验收流

**触发**：预约开始前场馆发现地胶起翘等故障，运营创建维护锁定（`POST /operator/maintenances`）。
服务在同一事务内锁定场地、并为覆盖到的未开场订单（paid/warming）生成**换场方案**
（`relocation_offers`）：筛选同时间可用场地（同场馆、容量够、时段无冲突、无维护/待验收阻断、
在营业/加场时间内），并快照每个候选场地相对原订单场地费的差价。

**用户三选一**（`POST /bookings/{id}/relocation-offer/accept`）：

| 选择 | 系统行为 |
| --- | --- |
| `relocate` 换场 | 旧单 → `relocated`（终态释放占位），新订单落到目标场地、**继承原价与已支付金额**；换到更高价场地时差价记 `venue_subsidy_cents` **由场馆承担**，用户不补差价 |
| `postpone` 顺延 | 同时长顺延到新时段（同场地，需已过验收/无维护阻断），同样旧单 `relocated` + 新订单继承款项 |
| `refund` 退款 | 旧单 → `venue_fault`，场馆原因全额退款并自动开争议单 |

- **价格**：场地支持单场地价格覆盖（`courts.price_cents_per_hour`，种子含 2 片 VIP 场地 90 元/小时），
  下单/报价/延时费/换场差价全部走同一计费引擎。
- **权限同步**：方案确认后旧入场码立即作废（闸机 `denied` 并提示换场），新订单生成新入场码；
  旧场地灯控关闭留痕，若已进预热窗口新场地直接预热（否则到点由调度器自动预热），
  `relocate.permission_synced` 事件与新场地 `light_records` 同时落库。
- **已进场清场**：若用户已扫码进入旧场地（有到场证据），锁定即生成**旧场地清场提醒**
  （`ops_notifications`，前台 `GET /front/notifications` 可见、`POST /front/notifications/{id}/ack` 确认清场），
  并把维护原因写入旧订单（`maintenance_reason`）；旧订单为 `relocated` 终态，不参与爽约裁决、
  不被前场延时逻辑误判为占场。
- **验收流**：维护完工需运营 `POST /operator/maintenances/{id}/finish` 上报（→ `acceptance_pending`），
  **前台验收** `POST /operator/maintenances/{id}/accept` 通过（→ `released`）后场地才重新开放预约；
  验收不通过回到 `active` 继续维护。`acceptance_pending` 期间该场地所有后续时段均不可约。
- **不响应兜底**：用户开场前未确认方案，开场后 20 分钟裁决任务按维护证据判 `venue_fault`
  自动全额退款，方案自动置 `expired`，不会误判爽约。

---

## 5. 主要接口（完整定义见 Swagger）

- 认证：`POST /auth/register`、`/auth/login/member`、`/auth/login/staff`
- 目录：`GET /venues`、`GET /venues/{id}/courts`、`GET /venues/{id}/schedule?court_id=&day=`、`POST /quotes`
- 预约：`POST /bookings`、`GET /bookings/{id}`、`POST /bookings/{id}/pay|cancel|quote-cancel`
- 换场：`GET /bookings/{id}/relocation-offer`、`POST /bookings/{id}/relocation-offer/accept`、`GET /me/relocation-offers`
- 现场：`.../checkin`、`.../light`、`.../session/start|end`、`.../participants`、`.../headcount`、`.../waive-overlimit`、`.../compensation`、`.../refund-requests`
- 客服：`GET .../timeline`、`GET .../verdict`、`GET/POST /cs/disputes`、`POST /cs/disputes/{id}/resolve`
- 运营：`POST/GET /operator/maintenances`、`.../finish`、`.../accept`（前台验收）、`GET /operator/relocation-offers`、`POST /operator/extra-slots`、`POST /operator/rules/cancellation`
- 前台待办：`GET /front/notifications`、`POST /front/notifications/{id}/ack`
- 设备：`POST /devices/gate/scan`、`POST /devices/light/{bookingId}`
- 内部（演示/运维）：`POST /internal/jobs/expire-unpaid|auto-light|judge-noshow`、`GET/POST /internal/clock`

---

## 6. 本题验证方式与已执行的验证结论

**实际执行过的验证**（在本仓库当前代码上运行，非推测）：

- 服务对一个干净数据库启动，自动迁移（0001 基础表 + 0002 换场/验收/通知扩展，含 btree_gist 与
  GiST 排他约束）与种子数据成功；
- `bash scripts/local-e2e.sh`（等价于 verify compose + 脚本）端到端 **96 项断言全部通过（FAIL=0）**，覆盖 15 个场景：
  1. 8 并发抢同一场地仅 1 单成功，其余 7 单被 Redis 锁/GiST 约束拒绝；
  2. 支付超时释放场地且可重新预订；**已进入灯控预热(warming)的订单不被超时任务回收**；
  3. 闸机按时间窗口放行（太早拒绝、开场前 20 分钟内放行）；
  4. 不到场自动判爽约并扣次数、自动开争议单，客服撤销爽约后次数返还、verdict=excused，timeline 证据完整；
  5. 场地临时维护为未开场订单生成换场方案（订单保持 paid、维护原因写入订单），用户选择退款后
     venue_fault 全额退款；维护时段新预约被拒；
  6. 人工开灯但无人上场 → ambiguous 进争议单（自动开灯不构成到场证据）；
  7. 前一场延时 10 分钟：闸机 `waiting`、前一场按 15 分钟计延时费、后一场自动补 10 分钟且在补偿内结束不另收费；
     另验证无补偿超时 25 分钟按 30 分钟计费；
  8. 拼场超员 2 人计 4000 分，前台减免后归零；
  9. 金卡高峰原价 10000、8 折后 8000、60 分钟免费时长全额抵扣、应付 0（高峰时段按星期动态选择，日期无关）；
  10. 高峰提前 ≥24h 退订退 80%，幂等键重试返回同一订单；
  11. 普通会员在 24h 会员优先窗口外抢高峰被 `MEMBER_ONLY_PEAK` 拒绝；
  12. 23:00–23:30 临时加场在排期中可见、可下单，加场外的时段仍 `OUTSIDE_HOURS`；
  13. 前台发 30 分钟未来补偿券可用于新单抵扣；客服 mixed 处理（部分退 1000 + 补偿 30 分钟）入账正确；
  14. 用户有退款申请但无到场证据 → ambiguous 人工单，客服驳回后争议单 resolved；
  15. **场地维护临时换场**：已扫码进场用户触发清场提醒（前台可查可确认）；换场到 VIP 高价场地，
      差价 3000 分由场馆承担、用户不补差价；旧入场码作废、新入场码闸机放行、新场地灯控预热同步；
      维护完工 → 前台验收（不通过回炉）→ 验收通过才重新开放；顺延到同时长新时段；
      用户不响应时裁决兜底 venue_fault 全额退款、方案自动失效、不计爽约。
- Swagger UI 与 `/swagger/openapi.yaml` 均返回 200，OpenAPI 文档通过结构检查（39 paths / 41 operations）。
- `go vet ./...` 与 `gofmt` 检查通过，`go build -mod=vendor ./...` 全绿。

**复现实验**：

```bash
make e2e                                   # 本地用户态 PG + 本机 Redis，一键跑 96 项断言
# 或
docker compose -f docker-compose.yml -f docker-compose.verify.yml up -d --build
BASE=http://localhost:8080 bash scripts/e2e_test.sh
```

脚本通过虚拟时钟（`ALLOW_TEST_CLOCK`）把"开场、预热、超时、爽约裁决、延时结束"压缩到几十秒内确定性执行，
并在每个需要后台处理的节点显式调用 `/internal/jobs/*`，因此可重复运行（需在干净数据库上，脚本/`local-e2e.sh`
会自动 drop & recreate 库并 `FLUSHALL` Redis）。

> 说明：本环境 Docker 守护进程不可用，因此镜像未在本机实际 `docker build`，但 Dockerfile 使用与本地验证相同的
> Go 1.22 工具链与已 vendor 的依赖（多阶段构建、distroless 运行时）；compose 配置已通过 YAML 解析校验。
> 业务逻辑已在本机用真实 PostgreSQL 15 + Redis 7 完成上述全部端到端验证。

---

## 7. 目录结构

```
cmd/server/            程序入口（迁移、种子、HTTP、后台任务 ticker）
internal/
  config/              环境变量配置
  store/               pgx 连接池、迁移(embed SQL)、Redis 分布式锁、数据模型
    migrations/        0001_init.sql（全部基础表、约束、GiST 排他索引）
                       0002_relocate.sql（临时换场/维护验收/清场通知/场地级价格）
  auth/                HMAC token、bcrypt
  service/             领域逻辑：报价/预约/支付/灯控/闸机/裁决/退订/维护/换场/争议/排期
  api/                 HTTP 路由与处理器
    web/swagger/       内置 Swagger UI 静态资源 + openapi.yaml
  seed/                演示种子数据（含 2 片 VIP 高价场地）
scripts/
  e2e_test.sh          15 场景 / 96 断言端到端验证
  local-e2e.sh         无 Docker 的一键本地验证
docker-compose.yml             一键启动（app + postgres + redis）
docker-compose.verify.yml      验证覆盖配置（关闭自动后台任务）
Dockerfile                     多阶段构建，distroless 运行时
```
