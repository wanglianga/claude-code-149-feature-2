# 城市体育馆 · 羽毛球场地预约 / 灯控 / 计费 / 爽约处罚服务

Go + PostgreSQL + Redis 实现的后端服务。覆盖预约下单、支付与超时释放、闸机放行、灯控预热/开灯/关灯、
前台签到、延时与拼场超员计费、退订规则、爽约/迟到/场馆原因自动裁决、场地维护锁定与临时换场（换场/顺延/退款、
差价场馆承担、闸机灯控权限同步、旧场清场提醒、维修完工前台验收）、临时加场、
会员权益抵扣与客服争议处理。所有状态变化都写入统一事件日志，客服可查看「预约 + 支付 + 闸机 + 灯控 +
前台 + 维护 + 换场权限同步 + 清场」完整时间线，而不必只按订单表猜测。

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

服务首次启动会自动建表并写入种子数据（1 个场馆、8 片场地、3 个会员、3 个员工、高峰窗口与退订规则）。
8 片场地为同价场地；可用 `POST /api/v1/operator/courts/{id}/price` 把某片设为高价（如 VIP），用于演示换场差价。

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
held/paid/warming/in_use 遇场地维护锁定 ─▶ relocation_pending
  ├─ 用户选 move（换同时段场地，高价差价场馆承担/低价差额退还）─▶ 旧单 moved + 新单 paid/warming
  ├─ 用户选 postpone（顺延到其他时间/场地）                  ─▶ 旧单 moved + 新单 paid
  ├─ 用户选 refund（或逾期未选，系统自动）                    ─▶ venue_fault（全额退款 + 开争议单）
  └─ 换场确认：闸机/灯控权限旧场 revoke、新场 grant；已进入旧场则生成清场提醒防误判占场
paid/warming 开场后 20min 仍无到场证据 ─▶ no_show（扣爽约次数 + 自动开争议单）
证据不足（有退款申请/灯开了未上场） ─▶ disputed（客服人工裁决）
场地维护：active ──维修完工──▶ repair_done（仍锁定）──前台验收通过──▶ verified（重新开放预约）
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
| 场地临修 | `/operator/maintenances` | 锁定排期；held 释放，paid 等进入换场流程（换场/顺延/退款），见 4.7 |
| 临时加场 | `/operator/extra-slots` | 营业时段外开放，排期接口可见可约 |

每个动作都同时写对应业务表（gate/light/front/payments/maintenance…）与统一 `event_log`（含 source、actor、payload），
`GET /bookings/{id}/timeline` 一次返回客服裁决所需的全部证据。

### 4.7 场地维护临时换场

预约开始前发现地胶起翘等问题时，运营用 `/operator/maintenances` 锁定场地：

- `held` 待支付订单直接过期释放（释放权益预占）；
- `paid/warming/in_use` 已支付订单进入 **`relocation_pending`**（不再直接判 venue_fault），
  订单 `note` 写入维护原因与方案确认截止时间（开场前 10 分钟；临开场锁定给 5 分钟宽限）。

用户（或员工协助）有三种选择，通过 `POST /bookings/{id}/relocation/resolve` 确认：

| choice | 含义 | 结果 |
| --- | --- | --- |
| `move` | 换至**同时段**其他可用场地 | 旧单 `moved`，新建 paid/warming 订单（携带原付款、到场证据与补偿时长） |
| `postpone` | 顺延到其他**时间 + 场地** | 旧单 `moved`，新时段新场地新建 paid 订单 |
| `refund` | 不换，场馆原因全额退款 | 旧单 `venue_fault`，全额退款 + 自动开争议单 |

- **候选筛选**：`GET /bookings/{id}/relocation/options`（顺延可带 `start_time/end_time`）只返回同场馆、容量足够、
  营业/加场时段内、无重叠预约且无维护锁定的场地，并逐场给出差价（按会员折后且沿用原权益分钟的场地费计算）：
  - 换到更贵场地：`price_diff_cents>0` → `venue_borne_cents` **由场馆承担**（payments 记 `compensation/venue_borne`），会员不加价；
  - 换到更便宜场地：差额 `refund` 退还会员。
  - 场地可设置差异化平时价：`POST /operator/courts/{id}/price`（分/小时，0 恢复场馆基础价）。
- **闸机/灯控权限同步**：方案确认即写 `device_permission_sync`——旧场地 gate/light 各一条 `revoke`、
  新场地各一条 `grant`；旧入场码再扫码被拒并引导用新码，新码在新场地放行（timeline 可查同步记录）。
- **已进入旧场地的清场**：若会员已有闸机/签到到场证据，系统生成旧场地**清场提醒**（`vacate_reminders`，
  前台可 `GET /front/vacate-reminders` 查看并在用户离场后 `.../clear`），维护原因写入旧订单 `note`，
  旧单置 `moved`、爽约裁决跳过换场链路订单，避免被误判为用户占场/爽约；到场证据复制到新单。
- **维修完工 → 前台验收**：维护单 `active` → 运营 `/repair-complete` 置 `repair_done`（场地仍锁定、仍不可订）→
  必须前台 `/verify` 通过才置 `verified` 并重新开放预约；验收不通过可退回 `active` 重修。
- **逾期兜底**：超过确认截止仍未选择的订单，由后台任务 `/internal/jobs/relocation-timeout`
  （进程内随 ticker 周期执行）按场馆原因自动全额退款，不长期悬挂占场。

---

## 5. 主要接口（完整定义见 Swagger）

- 认证：`POST /auth/register`、`/auth/login/member`、`/auth/login/staff`
- 目录：`GET /venues`、`GET /venues/{id}/courts`、`GET /venues/{id}/schedule?court_id=&day=`、`POST /quotes`
- 预约：`POST /bookings`、`GET /bookings/{id}`、`POST /bookings/{id}/pay|cancel|quote-cancel`
- 现场：`.../checkin`、`.../light`、`.../session/start|end`、`.../participants`、`.../headcount`、`.../waive-overlimit`、`.../compensation`、`.../refund-requests`
- 临时换场：`GET .../relocation/options`（筛选同时间/顺延候选与差价）、`POST .../relocation/resolve`（move/postpone/refund）
- 客服：`GET .../timeline`、`GET .../verdict`、`GET/POST /cs/disputes`、`POST /cs/disputes/{id}/resolve`
- 运营：`POST/GET /operator/maintenances`、`.../release`、`.../repair-complete`、`.../verify`、
  `POST /operator/extra-slots`、`POST /operator/courts/{id}/price`、`POST /operator/rules/cancellation`
- 前台清场：`GET /front/vacate-reminders`、`POST /front/vacate-reminders/{id}/clear`
- 设备：`POST /devices/gate/scan`、`POST /devices/light/{bookingId}`
- 内部（演示/运维）：`POST /internal/jobs/expire-unpaid|auto-light|judge-noshow|relocation-timeout`、`GET/POST /internal/clock`

---

## 6. 本题验证方式与已执行的验证结论

**实际执行过的验证**（在本仓库当前代码上运行，非推测）：

- 服务对一个干净数据库启动，自动迁移（0001 建 20+ 张表含 btree_gist/GiST 排他约束；
  0002 增量加换场字段、`device_permission_sync`、`vacate_reminders` 与维护验收状态）与种子数据成功；
  另验证**仅含 0001 的旧库在新服务启动时平滑增量迁移到 0002，存量订单/维护数据无损且可走新换场流程**；
- `bash scripts/local-e2e.sh`（等价于 verify compose + 脚本）端到端 **109 项断言全部通过（FAIL=0）**，覆盖 20 个场景。
  原 14 个场景保持通过（其中场景 5 改为走新换场流程的退款选择、场景 9 改为与运行星期无关的工作日高峰），
  新增临时换场 6 个场景（15–20）：
  1. 8 并发抢同一场地仅 1 单成功，其余 7 单被 Redis 锁/GiST 约束拒绝；
  2. 支付超时释放场地且可重新预订；**已进入灯控预热(warming)的订单不被超时任务回收**；
  3. 闸机按时间窗口放行（太早拒绝、开场前 20 分钟内放行）；
  4. 不到场自动判爽约并扣次数、自动开争议单，客服撤销爽约后次数返还、verdict=excused，timeline 证据完整；
  5. 场地临修后已支付订单进入 `relocation_pending`，用户选退款 → venue_fault 全额退款，维护期新预约被拒；
  6. 人工开灯但无人上场 → ambiguous 进争议单（自动开灯不构成到场证据）；
  7. 前一场延时 10 分钟：闸机 `waiting`、前一场按 15 分钟计延时费、后一场自动补 10 分钟且在补偿内结束不另收费；
     另验证无补偿超时 25 分钟按 30 分钟计费；
  8. 拼场超员 2 人计 4000 分，前台减免后归零；
  9. 金卡工作日高峰原价 10000、8 折后 8000、60 分钟免费时长全额抵扣、应付 0；
  10. 高峰提前 ≥24h 退订退 80%，幂等键重试返回同一订单；
  11. 普通会员在 24h 会员优先窗口外抢高峰被 `MEMBER_ONLY_PEAK` 拒绝；
  12. 23:00–23:30 临时加场在排期中可见、可下单，加场外的时段仍 `OUTSIDE_HOURS`；
  13. 前台发 30 分钟未来补偿券可用于新单抵扣；客服 mixed 处理（部分退 1000 + 补偿 30 分钟）入账正确；
  14. 用户有退款申请但无到场证据 → ambiguous 人工单，客服驳回后争议单 resolved；
  15. **维护锁定 → 换至同时段同价场地**：旧单 moved/verdict=moved、新单 paid 关联原场地、旧码闸机拒绝+新码放行、
      闸机与灯控各 2 条权限同步（旧 revoke/新 grant）；维修完工 `repair_done` 待验收期仍不可订，前台验收不通过退回、
      通过后 `verified` 重开；爽约裁决不把 moved 旧单判爽约、新单 on_time；
  16. **已扫码进入旧场地 → 换至高价场地**：6000 差价由场馆承担（会员不加价，记 venue_borne 补偿 + 原付款结转），
      生成旧场清场提醒（含"不计占场"提示）、前台确认后 cleared，新单迁移到场证据 verdict=on_time 不误判占场；
  17. **顺延**到其他时间/场地：候选按 13:00 时段筛选，旧单 moved、新单 paid 且新码在新时段闸机放行；
  18. 用户主动选 **refund**：场馆原因全额退款、venue_fault、维护原因写入旧单 note、自动开争议单；
  19. **逾期未确认**：超过截止由 `/internal/jobs/relocation-timeout` 自动全额退款（未到截止不处理）；
  20. **高价换低价**：6000 差额退还会员（新单 refunded_cents=6000），场地专属价可重置回场馆基础价。
- Swagger UI 与 `/swagger/openapi.yaml` 均返回 200（`/swagger/`、bundle 200，YAML 字节与仓库文件一致），
  OpenAPI 覆盖 Go 全部业务路由（41 paths），新增换场/验收/清场/场地定价接口与 Booking 新字段均已文档化。
- `go vet ./...` 与 `gofmt` 检查通过，`go build -mod=vendor ./...` 全绿。

**复现实验**：

```bash
make e2                                   # 本地用户态 PG + 本机 Redis，一键跑 109 项断言
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
    migrations/        0001_init.sql（全部表、约束、GiST 排他索引）
  auth/                HMAC token、bcrypt
  service/             领域逻辑：报价/预约/支付/灯控/闸机/裁决/退订/维护/换场/争议/排期
    其中 relocation.go 为临时换场（候选/换场/顺延/退款/权限同步/清场/逾期兜底），ops.go 含维护验收流转
  api/                 HTTP 路由与处理器
    web/swagger/       内置 Swagger UI 静态资源 + openapi.yaml
  seed/                演示种子数据
scripts/
  e2e_test.sh          20 场景 / 109 断言端到端验证
  local-e2e.sh         无 Docker 的一键本地验证
docker-compose.yml             一键启动（app + postgres + redis）
docker-compose.verify.yml      验证覆盖配置（关闭自动后台任务）
Dockerfile                     多阶段构建，distroless 运行时
```
