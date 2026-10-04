#!/usr/bin/env bash
# 端到端验证：预约防重 / 支付超时 / 灯控预热 / 闸机窗口 / 爽约 / 场馆原因 /
# 灯开未上场 / 前一场延时补偿 / 拼场超员 / 会员权益 / 高峰退订 / 临时加场 / 客服争议 /
# 场地维护临时换场（换场/顺延/退款 + 前台验收 + 清场提醒 + 未响应兜底）
# 说明：服务以 ALLOW_TEST_CLOCK=1 DISABLE_BG_JOBS=1 启动，脚本通过虚拟时钟+显式触发后台任务保证确定性。
# 高峰相关断言按“明天/D2 是星期几”动态选择高峰时段（周末 10:00 / 工作日 19:00），日期无关可重复。
set -u
BASE="${BASE:-http://127.0.0.1:18080}"
PASS=0; FAIL=0

# D0 = 明天 00:00 UTC（脚本运行当天服务可用；明天一定在营业）
TOMORROW=$(date -u -d 'tomorrow' +%Y-%m-%d)
BASE0=$(date -u -d "$TOMORROW 00:00" +%s)
# D2 = D0 + 2 天（用于 48h 会员优先窗口验证）
D2=$(date -u -d "$TOMORROW +2 days" +%Y-%m-%d)
# 高峰时段按星期动态选择：周末高峰 09-12/14-17，工作日高峰 18-21。
# 这样无论明天是工作日还是周末，断言都稳定。
DOW=$(date -u -d "$TOMORROW" +%u)          # 1=周一 .. 7=周日
if [ "$DOW" -ge 6 ]; then PEAK_H=10; else PEAK_H=19; fi
D2_DOW=$(date -u -d "$D2" +%u)
if [ "$D2_DOW" -ge 6 ]; then D2_PEAK_H=10; else D2_PEAK_H=19; fi
T_D2_PEAK=$(date -u -d "$D2 $D2_PEAK_H:00" +%s)

hm() { echo $(( BASE0 + $1*3600 )); }                 # 当天第 N 小时
iso() { date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ; }
adv() { curl -s -X POST "$BASE/internal/clock" -H 'Content-Type: application/json' -d "{\"offset_seconds\":$(( $1 - $(date +%s) ))}" >/dev/null; }
advReal() { curl -s -X POST "$BASE/internal/clock" -H 'Content-Type: application/json' -d "{\"offset_seconds\":$1}" >/dev/null; }
job_expire() { curl -s -X POST "$BASE/internal/jobs/expire-unpaid"; echo; }
job_light()  { curl -s -X POST "$BASE/internal/jobs/auto-light" >/dev/null; }
job_judge()  { curl -s -X POST "$BASE/internal/jobs/judge-noshow" >/dev/null; }

api() {
  local m="$1" p="$2" t="${3:-}" body="${4:-}"
  if [ -n "$body" ]; then
    curl -s -X "$m" "$BASE$p" -H 'Content-Type: application/json' ${t:+-H "Authorization: Bearer $t"} -d "$body"
  else
    curl -s -X "$m" "$BASE$p" ${t:+-H "Authorization: Bearer $t"}
  fi
}
check() { if [ "$2" == "$3" ]; then echo "PASS  $1 ($3)"; PASS=$((PASS+1)); else echo "FAIL  $1 期望[$2] 实际[$3]"; FAIL=$((FAIL+1)); fi; }
truthy() { if [ "$2" == "true" ]; then echo "PASS  $1"; PASS=$((PASS+1)); else echo "FAIL  $1 实际[$2]"; FAIL=$((FAIL+1)); fi; }

login_member() { api POST /api/v1/auth/login/member "" "$(jq -nc --arg p "$1" '{phone:$p,password:"123456"}')" | jq -r .token; }
login_staff() { api POST /api/v1/auth/login/staff "" "$(jq -nc --arg u "$1" --arg p "$2" '{username:$u,password:$p}')" | jq -r .token; }
reg() { api POST /api/v1/auth/register "" "$(jq -nc --arg p "$1" --arg n "$2" '{phone:$p,name:$n,password:"123456"}')" >/dev/null; login_member "$1"; }
pay() { api POST /api/v1/bookings/$1/pay "$2" '{}' >/dev/null; }
mk() { # token court start end [extra]
  local x="${5:-}"
  [ -z "$x" ] && x='{}'
  api POST /api/v1/bookings "$1" "$(jq -nc --argjson c "$2" --arg s "$(iso $3)" --arg e "$(iso $4)" \
    --argjson x "$x" \
    '{venue_id:1,court_id:$c,start_time:$s,end_time:$e,party_size:($x.party_size//4),
      rent_racket:($x.rent_racket==true),racket_count:($x.racket_count//0),
      buy_shuttle:($x.buy_shuttle==true),shuttle_count:($x.shuttle_count//0),
      use_benefit:($x.use_benefit!=false),idempotency_key:($x.idempotency_key//null),note:($x.note//"")}')"
}
bid() { jq -r '.booking.id'; }

OPS=$(login_staff operator operator123); FRONT=$(login_staff front front123); CS=$(login_staff cs cs123)
GOLD=$(login_member 13800000003)
T_CONC=$(reg 13900001001 并发会员); T_GATE=$(reg 13900001002 闸机会员)
T_NS=$(reg 13900001003 爽约会员); T_LIGHT=$(reg 13900001004 灯光会员)
T_OVR=$(reg 13900001005 延时会员); T_OL=$(reg 13900001006 超员会员)
T_RL1=$(reg 13900001011 换场会员); T_RL2=$(reg 13900001012 顺延会员)
T_RL4=$(reg 13900001014 未响应会员)

echo "================ 场景 1：并发预约同一场地，只能成功一单 ================"
S1=$(hm 8); E1=$(( S1+3600 )); adv $(( S1-1200 ))
BODY=$(jq -nc --arg s "$(iso $S1)" --arg e "$(iso $E1)" '{venue_id:1,court_id:1,start_time:$s,end_time:$e,party_size:4}')
seq 1 8 | xargs -P 8 -I{} curl -s -X POST "$BASE/api/v1/bookings" -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $T_CONC" -d "$BODY" > /tmp/conc.out
check "并发 8 单成功数=1" 1 "$(grep -c '"booking_no"' /tmp/conc.out)"
check "并发 8 单被拒数=7" 7 "$(grep -cE 'SLOT_LOCKED|SLOT_TAKEN' /tmp/conc.out)"
check "GiST 兜底错误类型出现(SLOT_LOCKED/SLOT_TAKEN)" true "$(grep -qE 'SLOT_LOCKED|SLOT_TAKEN' /tmp/conc.out && echo true)"
adv $(( S1+16*60 )); job_expire >/dev/null   # 清理场景1的待支付单

echo "================ 场景 2：支付超时释放；已预热订单独立状态不释放 ================"
S2=$(hm 9); E2=$(( S2+3600 )); adv $(( S2-10*60 ))
B2=$(mk "$T_CONC" 2 "$S2" "$E2" | bid)
adv $(( S2+6*60 )); check "过期扫描释放 1 单" '{"expired":1}' "$(job_expire)"
check "未支付订单 -> expired" expired "$(api GET /api/v1/bookings/$B2 "$T_CONC" | jq -r .status)"
# 释放后同场地时段可重新被订（虚拟时钟回到开场前）
adv $(( S2-5*60 ))
B2X=$(mk "$T_GATE" 2 "$S2" "$E2" | bid); pay $B2X "$T_GATE"
check "释放后场地可重新预约并支付" paid "$(api GET /api/v1/bookings/$B2X "$T_GATE" | jq -r .status)"

S2B=$(hm 11); E2B=$(( S2B+3600 ))
B2B=$(mk "$T_CONC" 3 "$S2B" "$E2B" | bid); pay $B2B "$T_CONC"
adv $(( S2B-9*60 )); job_light
check "支付后进入灯控预热 warming" warming "$(api GET /api/v1/bookings/$B2B "$T_CONC" | jq -r .status)"
check "超时扫描不回收预热订单" '{"expired":0}' "$(job_expire)"
check "预热订单仍保持 warming" warming "$(api GET /api/v1/bookings/$B2B "$T_CONC" | jq -r .status)"
api POST /api/v1/bookings/$B2B/cancel "$OPS" '{"reason":"验证后清理"}' >/dev/null

echo "================ 场景 3：闸机按时间窗口放行 ================"
S3=$(hm 12); E3=$(( S3+1800 )); adv $(( S3-1800 ))
B3=$(mk "$T_GATE" 4 "$S3" "$E3" | bid); pay $B3 "$T_GATE"
CODE3=$(api GET /api/v1/bookings/$B3 "$T_GATE" | jq -r .entry_code)
adv $(( S3-3600 ))
check "开场前 1 小时闸机拒绝" denied "$(api POST /api/v1/devices/gate/scan "" "$(jq -nc --arg c "$CODE3" '{entry_code:$c,device_id:"G1"}')" | jq -r .result)"
adv $(( S3-10*60 ))
check "开场前 10 分钟闸机放行" allowed "$(api POST /api/v1/devices/gate/scan "" "$(jq -nc --arg c "$CODE3" '{entry_code:$c,device_id:"G1"}')" | jq -r .result)"

echo "================ 场景 4：不到场 -> 爽约；客服撤销爽约并返还次数 ================"
S4=$(hm 13); E4=$(( S4+3600 )); adv $(( S4-1200 ))
B4=$(mk "$T_NS" 5 "$S4" "$E4" | bid); pay $B4 "$T_NS"
adv $(( S4+25*60 )); job_judge
check "未到场订单裁决 -> no_show" no_show "$(api GET /api/v1/bookings/$B4 "$T_NS" | jq -r .status)"
check "裁决依据 verdict=no_show" no_show "$(api GET /api/v1/bookings/$B4 "$T_NS" | jq -r .verdict)"
check "爽约扣 1 次" 1 "$(api POST /api/v1/auth/login/member "" '{"phone":"13900001003","password":"123456"}' | jq -r .member.no_show_count)"
DISP=$(api GET "/api/v1/cs/disputes?status=open" "$CS" | jq "[.disputes[]|select(.booking_id==$B4)][0].id")
api POST /api/v1/cs/disputes/$DISP/resolve "$CS" '{"action":"rescind_noshow","note":"电话核实，撤销爽约"}' >/dev/null
check "撤销后爽约次数回 0" 0 "$(api POST /api/v1/auth/login/member "" '{"phone":"13900001003","password":"123456"}' | jq -r .member.no_show_count)"
check "撤销后 verdict=excused" excused "$(api GET /api/v1/bookings/$B4 "$T_NS" | jq -r .verdict)"
TL=$(api GET /api/v1/bookings/$B4/timeline "$CS")
truthy "timeline 含支付/闸机/事件记录" "$(echo "$TL" | jq '(.payments|length)>=1 and (.events|length)>=3')"

echo "================ 场景 5：场地临时维护 -> 生成换场方案；用户选退款 -> 场馆原因全额退款 ================"
S5=$(hm 13); S5=$(( S5+30*60 )); E5=$(( S5+3600 )); adv $(( S5-1200 ))
B5=$(mk "$T_NS" 6 "$S5" "$E5" | bid); pay $B5 "$T_NS"
PAID5=$(api GET /api/v1/bookings/$B5 "$T_NS" | jq -r .paid_cents)
M5=$(api POST /api/v1/operator/maintenances "$OPS" "$(jq -nc --arg s "$(iso $S5)" --arg e "$(iso $E5)" '{venue_id:1,court_id:6,start_time:$s,end_time:$e,reason:"地胶维修"}')")
check "维护锁定为未开场订单生成换场方案" 1 "$(echo "$M5" | jq '.offers|length')"
check "方案待确认时订单保持 paid" paid "$(api GET /api/v1/bookings/$B5 "$T_NS" | jq -r .status)"
check "维护原因已写入订单(防误判占场)" 地胶维修 "$(api GET /api/v1/bookings/$B5 "$T_NS" | jq -r .maintenance_reason)"
check "维护时段新预约被拒 MAINTENANCE" MAINTENANCE "$(mk "$T_NS" 6 "$S5" "$E5" | jq -r '.error.code // empty')"
api POST /api/v1/bookings/$B5/relocation-offer/accept "$T_NS" '{"action":"refund"}' >/dev/null
check "用户选择退款 -> venue_fault" venue_fault "$(api GET /api/v1/bookings/$B5 "$T_NS" | jq -r .status)"
check "场馆原因全额退款(=已付 $PAID5)" "$PAID5" "$(api GET /api/v1/bookings/$B5 "$T_NS" | jq -r .refunded_cents)"

echo "================ 场景 6：人工开灯但无人上场 -> ambiguous 争议 ================"
S6=$(hm 15); E6=$(( S6+3600 )); adv $(( S6-1200 ))
B6=$(mk "$T_LIGHT" 7 "$S6" "$E6" | bid); pay $B6 "$T_LIGHT"
adv $(( S6-9*60 )); job_light
api POST /api/v1/devices/light/$B6 "" '{"action":"on","source":"device","detail":"现场误开"}' >/dev/null
adv $(( S6+25*60 )); job_judge
check "灯开未上场 -> ambiguous" ambiguous "$(api GET /api/v1/bookings/$B6/verdict "$FRONT" | jq -r .verdict)"
check "订单进入 disputed" disputed "$(api GET /api/v1/bookings/$B6 "$T_LIGHT" | jq -r .status)"

echo "================ 场景 7：前一场延时 -> 场边等待 -> 自动补偿 -> 延时结算 ================"
A7=$(hm 17); A7E=$(( A7+3600 )); B7S=$A7E; B7E=$(( B7S+3600 )); adv $(( A7-1200 ))
BA=$(mk "$T_OVR" 8 "$A7" "$A7E" | bid); BB=$(mk "$T_OVR" 8 "$B7S" "$B7E" | bid)
pay $BA "$T_OVR"; pay $BB "$T_OVR"
CODEB=$(api GET /api/v1/bookings/$BB "$T_OVR" | jq -r .entry_code)
adv $(( A7-9*60 )); job_light
adv "$A7"; job_light
check "前一场已 in_use" in_use "$(api GET /api/v1/bookings/$BA "$T_OVR" | jq -r .status)"
adv $(( B7S-10*60 ))
check "后一场到场但前一场延时 -> waiting" waiting "$(api POST /api/v1/devices/gate/scan "" "$(jq -nc --arg c "$CODEB" '{entry_code:$c,device_id:"G8"}')" | jq -r .result)"
adv $(( B7S+10*60 ))
api POST /api/v1/bookings/$BA/session/end "$FRONT" '{"method":"onsite"}' >/dev/null
check "前一场延时按 15 分钟计" 15 "$(api GET /api/v1/bookings/$BA "$T_OVR" | jq -r .overtime_minutes)"
truthy "前一场延时费>0" "$(api GET /api/v1/bookings/$BA "$T_OVR" | jq '.overtime_cents>0')"
api POST /api/v1/bookings/$BB/session/start "$FRONT" >/dev/null
check "后一场晚开 10 分钟" 10 "$(api GET /api/v1/bookings/$BB "$T_OVR" | jq -r .late_start_minutes)"
check "自动补偿 10 分钟" 10 "$(api GET /api/v1/bookings/$BB "$T_OVR" | jq -r .comp_bonus_minutes)"
adv $(( B7E+10*60 ))
api POST /api/v1/bookings/$BB/session/end "$FRONT" '{"method":"onsite"}' >/dev/null
BBJ=$(api GET /api/v1/bookings/$BB "$T_OVR")
check "补偿时长内结束无延时费" 0 "$(echo "$BBJ" | jq -r .overtime_minutes)"
check "后一场 completed" completed "$(echo "$BBJ" | jq -r .status)"
# 再多打 20 分钟（超出补偿 10 分钟）-> 应收延时费（另一场地新单演示）
S7X=$(hm 20); E7X=$(( S7X+3600 )); adv $(( S7X-1200 ))
B7X=$(mk "$T_OVR" 7 "$S7X" "$E7X" | bid); pay $B7X "$T_OVR"
api POST /api/v1/bookings/$B7X/checkin "$FRONT" >/dev/null
api POST /api/v1/bookings/$B7X/session/start "$FRONT" >/dev/null
adv $(( E7X+25*60 ))
api POST /api/v1/bookings/$B7X/session/end "$FRONT" '{"method":"onsite"}' >/dev/null
check "无补偿超时 25 分钟按 30 分钟计" 30 "$(api GET /api/v1/bookings/$B7X "$T_OVR" | jq -r .overtime_minutes)"

echo "================ 场景 8：会员带朋友拼场超员 -> 计费 -> 前台减免 ================"
S8=$(hm 19); E8=$(( S8+3600 )); adv $(( S8-1200 ))
B8=$(mk "$T_OL" 1 "$S8" "$E8" | bid); pay $B8 "$T_OL"
api POST /api/v1/bookings/$B8/participants "$T_OL" '{"name":"朋友A","phone":"13700000001"}' >/dev/null
api POST /api/v1/bookings/$B8/participants "$T_OL" '{"name":"朋友B","phone":"13700000002"}' >/dev/null
api POST /api/v1/bookings/$B8/headcount "$FRONT" '{"actual_count":8}' >/dev/null
B8J=$(api GET /api/v1/bookings/$B8 "$T_OL")
check "超员 2 人" 2 "$(echo "$B8J" | jq -r .overlimit_persons)"
check "超员费 4000 分(20 元/人)" 4000 "$(echo "$B8J" | jq -r .overlimit_cents)"
api POST /api/v1/bookings/$B8/waive-overlimit "$FRONT" >/dev/null
check "前台减免后超员费=0" 0 "$(api GET /api/v1/bookings/$B8 "$T_OL" | jq -r .overlimit_cents)"

echo "================ 场景 9：金卡 8 折 + 免费时长全额抵扣（高峰 ${PEAK_H}:00） ================"
S9=$(hm $PEAK_H); E9=$(( S9+3600 )); adv $(( S9-1200 ))
Q9=$(api POST /api/v1/quotes "" "$(jq -nc --arg s "$(iso $S9)" --arg e "$(iso $E9)" '{venue_id:1,member_id:3,start_time:$s,end_time:$e}')")
check "金卡高峰场地费原价 10000" 10000 "$(echo "$Q9" | jq -r .court_fee_cents)"
check "金卡 8 折折后场地费 8000" 8000 "$(echo "$Q9" | jq -r '.court_fee_cents-.discount_cents')"
B9=$(mk "$GOLD" 4 "$S9" "$E9" | bid); pay $B9 "$GOLD"
B9J=$(api GET /api/v1/bookings/$B9 "$GOLD")
check "金卡免费时长抵扣 60 分钟" 60 "$(echo "$B9J" | jq -r .benefit_minutes)"
check "抵扣后应付 0 分" 0 "$(echo "$B9J" | jq -r .payable_cents)"

echo "================ 场景 10：高峰退订规则（>=24h 退 80%）+ 幂等 ================"
adv $(( T_D2_PEAK-25*3600 ))   # 距开场 25 小时
E10=$(( T_D2_PEAK+3600 ))
KEY="idem-$(date +%s%N)"
R1=$(mk "$GOLD" 5 "$T_D2_PEAK" "$E10" "{\"idempotency_key\":\"$KEY\",\"use_benefit\":false}")
R2=$(mk "$GOLD" 5 "$T_D2_PEAK" "$E10" "{\"idempotency_key\":\"$KEY\",\"use_benefit\":false}")
check "幂等重试返回同一订单" "$(echo "$R1" | jq -r .booking.id)" "$(echo "$R2" | jq -r .booking.id)"
B10=$(echo "$R1" | bid); PAID10=$(echo "$R1" | jq -r .booking.payable_cents); pay $B10 "$GOLD"
check "高峰提前>=24h 退款比例 80%" 80 "$(api POST /api/v1/bookings/$B10/quote-cancel "$GOLD" | jq -r .refund_percent)"
CAN=$(api POST /api/v1/bookings/$B10/cancel "$GOLD" '{"reason":"临时有事"}')
check "实退金额=80%" $(( PAID10*80/100 )) "$(echo "$CAN" | jq -r .booking.refunded_cents)"
check "订单 cancelled" cancelled "$(echo "$CAN" | jq -r .booking.status)"

echo "================ 场景 11：普通会员抢 24h 外高峰被会员优先规则拒绝 ================"
check "普通会员超优先窗口 -> MEMBER_ONLY_PEAK" MEMBER_ONLY_PEAK \
  "$(mk "$T_GATE" 6 "$T_D2_PEAK" "$E10" | jq -r '.error.code // empty')"

echo "================ 场景 12：临时加场（当天 23:00-23:30 营业时段外可约） ================"
adv $(hm 22)
api POST /api/v1/operator/extra-slots "$OPS" "$(jq -nc --arg d "$TOMORROW" '{venue_id:1,slot_date:$d,opens_at:"23:00",closes_at:"23:30",note:"夜场加开"}')" >/dev/null
SCH=$(api GET "/api/v1/venues/1/schedule?court_id=7&day=$TOMORROW" "")
truthy "排期出现 23:00 加场空闲格" "$(echo "$SCH" | jq '[.slots[]|select(.start_time|endswith("23:00:00Z"))][0].status=="free"')"
T12=$(date -u -d "$TOMORROW 23:00" +%s); E12=$(( T12+1800 ))
B12=$(mk "$T_GATE" 7 "$T12" "$E12" | bid)
truthy "加场时段可下单" "$([ "${B12:-null}" != null ] && echo true)"
check "非加场且非营业时段 -> OUTSIDE_HOURS" OUTSIDE_HOURS "$(mk "$T_GATE" 7 "$((T12+1800))" "$((T12+3600))" | jq -r '.error.code // empty')"

echo "================ 场景 13：前台补偿券 + 客服 mixed 处理（部分退+补偿时长） ================"
api POST /api/v1/bookings/$B3/compensation "$FRONT" '{"minutes":30,"scope":"future","reason":"排队等候补偿"}' >/dev/null
GATE_UID=$(api POST /api/v1/auth/login/member "" '{"phone":"13900001002","password":"123456"}' | jq -r .member.id)
S13=$(hm 8); E13=$(( S13+3600 ))
Q13=$(api POST /api/v1/quotes "" "$(jq -nc --arg s "$(iso $S13)" --arg e "$(iso $E13)" --argjson m "$GATE_UID" '{venue_id:1,member_id:$m,start_time:$s,end_time:$e}')")
check "前台补偿券 30 分钟可用于抵扣" 30 "$(echo "$Q13" | jq -r .benefit_minutes_used)"
D6=$(api GET "/api/v1/cs/disputes?status=open" "$CS" | jq "[.disputes[]|select(.booking_id==$B6)][0].id")
api POST /api/v1/cs/disputes/$D6/resolve "$CS" '{"action":"mixed","refund_cents":1000,"comp_minutes":30,"note":"部分场馆责任"}' >/dev/null
check "争议单 -> resolved" resolved "$(api GET "/api/v1/cs/disputes?status=resolved" "$CS" | jq -r --argjson id "$D6" '[.disputes[]|select(.id==$id)][0].status')"
check "mixed 部分退款入账 1000" 1000 "$(api GET /api/v1/bookings/$B6/timeline "$CS" | jq '[.payments[]|select(.kind=="refund")]|map(.amount_cents)|add')"

echo "================ 场景 14：用户退款申请是裁决证据（有申请无到场 -> ambiguous 人工单） ================"
S14=$(hm 8); E14=$(( S14+3600 )); adv $(( S14-1200 ))
B14=$(mk "$T_GATE" 2 "$S14" "$E14" | bid); pay $B14 "$T_GATE"
api POST /api/v1/bookings/$B14/refund-requests "$T_GATE" '{"amount_cents":100,"reason":"路上堵车申请退款"}' >/dev/null
adv $(( S14+25*60 )); job_judge
check "有退款申请但无到场 -> ambiguous" ambiguous "$(api GET /api/v1/bookings/$B14/verdict "$FRONT" | jq -r .verdict)"
D14=$(api GET "/api/v1/cs/disputes?status=open" "$CS" | jq "[.disputes[]|select(.booking_id==$B14)][0].id")
api POST /api/v1/cs/disputes/$D14/resolve "$CS" '{"action":"reject","note":"规则内爽约不退"}' >/dev/null
check "客服驳回后争议单 resolved" resolved "$(api GET "/api/v1/cs/disputes?status=resolved" "$CS" | jq -r --argjson id "$D14" '[.disputes[]|select(.id==$id)][0].status')"

echo "================ 场景 15：场地维护临时换场（换场/顺延/验收/清场提醒/未响应兜底） ================"
# --- 15A 用户已扫码进旧场地 -> 换场到更高价 VIP 场地，差价场馆承担，权限同步新场地 ---
S15=$(hm 13); E15=$(( S15+3600 )); adv $(( S15-1800 ))          # 明天 13:00-14:00（任何星期都非高峰）
B15A=$(mk "$T_RL1" 3 "$S15" "$E15" | bid); pay $B15A "$T_RL1"
PAID15A=$(api GET /api/v1/bookings/$B15A "$T_RL1" | jq -r .paid_cents)   # 6000
CODE15A=$(api GET /api/v1/bookings/$B15A "$T_RL1" | jq -r .entry_code)
adv $(( S15-10*60 ))
check "换场前用户扫码进入旧场地" allowed "$(api POST /api/v1/devices/gate/scan "" "$(jq -nc --arg c "$CODE15A" '{entry_code:$c,device_id:"G3"}')" | jq -r .result)"
M15=$(api POST /api/v1/operator/maintenances "$OPS" "$(jq -nc --arg s "$(iso $S15)" --arg e "$(iso $((S15+5*3600)))" '{venue_id:1,court_id:3,start_time:$s,end_time:$e,reason:"地胶起翘"}')")
MID15=$(echo "$M15" | jq -r .maintenance.id)
check "维护生成 1 个换场方案" 1 "$(echo "$M15" | jq '.offers|length')"
check "已进场订单标记需要清场" true "$(echo "$M15" | jq '.offers[0].clearance_needed')"
check "方案待确认订单仍 paid" paid "$(api GET /api/v1/bookings/$B15A "$T_RL1" | jq -r .status)"
check "维护原因写入旧订单" 地胶起翘 "$(api GET /api/v1/bookings/$B15A "$T_RL1" | jq -r .maintenance_reason)"
NOTIF=$(api GET "/api/v1/front/notifications?status=pending" "$FRONT")
NID=$(echo "$NOTIF" | jq "[.notifications[]|select(.booking_id==$B15A and .type==\"clearance_reminder\")][0].id")
truthy "旧场地清场提醒已生成" "$([ "$NID" != null ] && echo true)"
OFFER15=$(api GET /api/v1/bookings/$B15A/relocation-offer "$T_RL1")
check "方案状态 pending" pending "$(echo "$OFFER15" | jq -r .status)"
check "候选含 VIP 场地 9 且差价 3000 场馆承担" 3000 "$(echo "$OFFER15" | jq '[.candidates[]|select(.court_id==9)][0].price_diff_cents')"
truthy "候选含同价普通场地(差价 0)" "$(echo "$OFFER15" | jq '[.candidates[]|select(.price_diff_cents==0)]|length>=1')"
ACC15=$(api POST /api/v1/bookings/$B15A/relocation-offer/accept "$T_RL1" '{"action":"relocate","court_id":9}')
NB15=$(echo "$ACC15" | jq -r .new_booking.id)
check "换场确认 -> accepted_relocate" accepted_relocate "$(echo "$ACC15" | jq -r .offer.status)"
check "高价差价 3000 由场馆承担" 3000 "$(echo "$ACC15" | jq -r .offer.venue_subsidy_cents)"
check "新订单在 VIP 场地 9" 9 "$(echo "$ACC15" | jq -r .new_booking.court_id)"
check "用户不补差价(应付仍 $PAID15A)" "$PAID15A" "$(echo "$ACC15" | jq -r .new_booking.payable_cents)"
check "已支付金额同步到新订单" "$PAID15A" "$(echo "$ACC15" | jq -r .new_booking.paid_cents)"
check "已进预热窗口新场地直接 warming" warming "$(echo "$ACC15" | jq -r .new_booking.status)"
check "旧订单 -> relocated" relocated "$(echo "$ACC15" | jq -r .old_booking.status)"
check "旧订单指向新订单" "$NB15" "$(echo "$ACC15" | jq -r .old_booking.superseded_by)"
check "旧入场码已作废" denied "$(api POST /api/v1/devices/gate/scan "" "$(jq -nc --arg c "$CODE15A" '{entry_code:$c,device_id:"G3"}')" | jq -r .result)"
CODE15N=$(api GET /api/v1/bookings/$NB15 "$T_RL1" | jq -r .entry_code)
check "新入场码闸机放行(权限已同步)" allowed "$(api POST /api/v1/devices/gate/scan "" "$(jq -nc --arg c "$CODE15N" '{entry_code:$c,device_id:"G9"}')" | jq -r .result)"
TLN=$(api GET /api/v1/bookings/$NB15/timeline "$CS")
truthy "灯控权限同步:新场地预热记录" "$(echo "$TLN" | jq '[.light_records[]|select(.court_id==9 and .action=="warmup")]|length>=1')"
truthy "权限同步事件留痕" "$(echo "$TLN" | jq '[.events[]|select(.event_type=="relocate.permission_synced")]|length>=1')"
truthy "款项迁移留痕(transfer $PAID15A)" "$(echo "$TLN" | jq --argjson p "$PAID15A" '[.payments[]|select(.method=="transfer")]|map(.amount_cents)|add==$p')"
TLO=$(api GET /api/v1/bookings/$B15A/timeline "$CS")
truthy "旧场地灯控关闭留痕" "$(echo "$TLO" | jq '[.light_records[]|select(.court_id==3 and .action=="off")]|length>=1')"
check "前台确认清场 -> done" done "$(api POST /api/v1/front/notifications/$NID/ack "$FRONT" | jq -r .status)"

# --- 15B 维护完工 -> 前台验收 -> 验收通过才重新开放；用户选择顺延 ---
adv $(( S15+70*60 ))                                            # 14:10，维护窗口内
api POST /api/v1/operator/maintenances/$MID15/finish "$OPS" >/dev/null
check "完工后 -> acceptance_pending" acceptance_pending "$(api GET /api/v1/operator/maintenances?venue_id=1 "$OPS" | jq -r --argjson id "$MID15" '[.maintenances[]|select(.id==$id)][0].status')"
check "待验收期间新预约仍被拒" MAINTENANCE "$(mk "$T_RL2" 3 "$((S15+2*3600))" "$((S15+3*3600))" | jq -r '.error.code // empty')"
api POST /api/v1/operator/maintenances/$MID15/accept "$FRONT" '{"pass":false,"note":"地胶仍起翘，继续维修"}' >/dev/null
check "验收不通过 -> 回到 active" active "$(api GET /api/v1/operator/maintenances?venue_id=1 "$OPS" | jq -r --argjson id "$MID15" '[.maintenances[]|select(.id==$id)][0].status')"
check "验收不通过场地仍锁定" MAINTENANCE "$(mk "$T_RL2" 3 "$((S15+2*3600))" "$((S15+3*3600))" | jq -r '.error.code // empty')"
api POST /api/v1/operator/maintenances/$MID15/finish "$OPS" >/dev/null
check "前台验收通过 -> released" released "$(api POST /api/v1/operator/maintenances/$MID15/accept "$FRONT" '{"pass":true,"note":"验收合格"}' | jq -r .status)"
B15B=$(mk "$T_RL2" 3 "$((S15+2*3600))" "$((S15+3*3600))" | bid)  # 15:00-16:00，验收通过后可约
truthy "验收通过后场地重新开放可约" "$([ "${B15B:-null}" != null ] && echo true)"
pay $B15B "$T_RL2"
M15B=$(api POST /api/v1/operator/maintenances "$OPS" "$(jq -nc --arg s "$(iso $((S15+2*3600)))" --arg e "$(iso $((S15+3*3600)))" '{venue_id:1,court_id:3,start_time:$s,end_time:$e,reason:"地胶再次起翘"}')")
MID15B=$(echo "$M15B" | jq -r .maintenance.id)
check "未进场订单无需清场" false "$(echo "$M15B" | jq -r '.offers[0].clearance_needed')"
api POST /api/v1/operator/maintenances/$MID15B/finish "$OPS" >/dev/null
api POST /api/v1/operator/maintenances/$MID15B/accept "$FRONT" '{"pass":true,"note":"验收合格"}' >/dev/null
check "顺延时长不一致被拒" BAD_DURATION "$(api POST /api/v1/bookings/$B15B/relocation-offer/accept "$T_RL2" "$(jq -nc --arg s "$(iso $((S15+3*3600)))" --arg e "$(iso $((S15+3*3600+1800)))" '{action:"postpone",start_time:$s,end_time:$e}')" | jq -r '.error.code // empty')"
ACC15B=$(api POST /api/v1/bookings/$B15B/relocation-offer/accept "$T_RL2" "$(jq -nc --arg s "$(iso $((S15+3*3600)))" --arg e "$(iso $((S15+4*3600)))" '{action:"postpone",start_time:$s,end_time:$e}')")
check "顺延确认 -> accepted_postpone" accepted_postpone "$(echo "$ACC15B" | jq -r .offer.status)"
check "顺延新订单同场地同时长(16:00-17:00)" "$(($S15+3*3600))" "$(echo "$ACC15B" | jq -r '.new_booking.start_time' | xargs -I{} date -u -d {} +%s)"
check "顺延订单保持 paid" paid "$(echo "$ACC15B" | jq -r .new_booking.status)"
check "顺延旧订单 -> relocated" relocated "$(echo "$ACC15B" | jq -r .old_booking.status)"

# --- 15C 用户不响应 -> 裁决兜底：场馆原因全额退款，方案自动失效，不误判占场 ---
adv $(( S15+100*60 ))
B15D=$(mk "$T_RL4" 10 "$((S15+2*3600))" "$((S15+3*3600))" | bid); pay $B15D "$T_RL4"
PAID15D=$(api GET /api/v1/bookings/$B15D "$T_RL4" | jq -r .paid_cents)
api POST /api/v1/operator/maintenances "$OPS" "$(jq -nc --arg s "$(iso $((S15+2*3600)))" --arg e "$(iso $((S15+3*3600)))" '{venue_id:1,court_id:10,start_time:$s,end_time:$e,reason:"地胶起翘"}')" >/dev/null
check "未响应用户方案已生成" pending "$(api GET /api/v1/bookings/$B15D/relocation-offer "$T_RL4" | jq -r .status)"
adv $(( S15+2*3600+25*60 )); job_judge
check "未响应 -> 场馆原因 venue_fault" venue_fault "$(api GET /api/v1/bookings/$B15D "$T_RL4" | jq -r .status)"
check "未响应 -> 全额退款(=已付 $PAID15D)" "$PAID15D" "$(api GET /api/v1/bookings/$B15D "$T_RL4" | jq -r .refunded_cents)"
check "未响应 -> 方案自动 expired" expired "$(api GET /api/v1/bookings/$B15D/relocation-offer "$T_RL4" | jq -r .status)"
check "未响应不计爽约(次数 0)" 0 "$(api POST /api/v1/auth/login/member "" '{"phone":"13900001014","password":"123456"}' | jq -r .member.no_show_count)"
check "换场用户不计爽约(次数 0)" 0 "$(api POST /api/v1/auth/login/member "" '{"phone":"13900001011","password":"123456"}' | jq -r .member.no_show_count)"

advReal 0
echo; echo "================================ 结果汇总 ================================"
echo "PASS=$PASS  FAIL=$FAIL"
[ $FAIL -eq 0 ]
