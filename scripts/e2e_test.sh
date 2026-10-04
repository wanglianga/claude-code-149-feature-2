#!/usr/bin/env bash
# 端到端验证：预约防重 / 支付超时 / 灯控预热 / 闸机窗口 / 爽约 / 场馆原因 /
# 灯开未上场 / 前一场延时补偿 / 拼场超员 / 会员权益 / 高峰退订 / 临时加场 / 客服争议 /
# 场地维护临时换场（换场/顺延/退款、差价场馆承担、闸机灯控权限同步、旧场清场提醒、维修完工前台验收、逾期兜底）
# 说明：服务以 ALLOW_TEST_CLOCK=1 DISABLE_BG_JOBS=1 启动，脚本通过虚拟时钟+显式触发后台任务保证确定性。
set -u
BASE="${BASE:-http://127.0.0.1:18080}"
PASS=0; FAIL=0

# D0 = 明天 00:00 UTC（脚本运行当天服务可用；明天一定在营业）
TOMORROW=$(date -u -d 'tomorrow' +%Y-%m-%d)
BASE0=$(date -u -d "$TOMORROW 00:00" +%s)
# D2 = D0 + 2 天（工作日，用于 48h 会员优先窗口验证）
D2=$(date -u -d "$TOMORROW +2 days" +%Y-%m-%d)
T_D2_19=$(date -u -d "$D2 19:00" +%s)

hm() { echo $(( BASE0 + $1*3600 )); }                 # 当天第 N 小时
iso() { date -u -d "@$1" +%Y-%m-%dT%H:%M:%SZ; }
adv() { curl -s -X POST "$BASE/internal/clock" -H 'Content-Type: application/json' -d "{\"offset_seconds\":$(( $1 - $(date +%s) ))}" >/dev/null; }
advReal() { curl -s -X POST "$BASE/internal/clock" -H 'Content-Type: application/json' -d "{\"offset_seconds\":$1}" >/dev/null; }
job_expire() { curl -s -X POST "$BASE/internal/jobs/expire-unpaid"; echo; }
job_light()  { curl -s -X POST "$BASE/internal/jobs/auto-light" >/dev/null; }
job_judge()  { curl -s -X POST "$BASE/internal/jobs/judge-noshow" >/dev/null; }
job_reloc()  { curl -s -X POST "$BASE/internal/jobs/relocation-timeout"; echo; }

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
T_MV=$(reg 13900001007 换场会员)

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

echo "================ 场景 5：场地临时维护 -> 换场流程中选择退款 -> 场馆原因全额退款；维护期不可订 ================"
S5=$(hm 13); S5=$(( S5+30*60 )); E5=$(( S5+3600 )); adv $(( S5-1200 ))
B5=$(mk "$T_NS" 6 "$S5" "$E5" | bid); pay $B5 "$T_NS"
PAID5=$(api GET /api/v1/bookings/$B5 "$T_NS" | jq -r .paid_cents)
api POST /api/v1/operator/maintenances "$OPS" "$(jq -nc --arg s "$(iso $S5)" --arg e "$(iso $E5)" '{venue_id:1,court_id:6,start_time:$s,end_time:$e,reason:"地胶维修"}')" >/dev/null
check "维护覆盖订单 -> relocation_pending" relocation_pending "$(api GET /api/v1/bookings/$B5 "$T_NS" | jq -r .status)"
check "用户选择退款 -> venue_fault" venue_fault "$(api POST /api/v1/bookings/$B5/relocation/resolve "$T_NS" '{"choice":"refund","note":"无法换场"}' | jq -r .old_booking.status)"
check "场馆原因全额退款(=已付 $PAID5)" "$PAID5" "$(api GET /api/v1/bookings/$B5 "$T_NS" | jq -r .refunded_cents)"
check "维护时段新预约被拒 MAINTENANCE" MAINTENANCE "$(mk "$T_NS" 6 "$S5" "$E5" | jq -r '.error.code // empty')"

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

echo "================ 场景 9：金卡 8 折 + 免费时长全额抵扣（工作日高峰 19:00，与运行星期无关） ================"
S9=$T_D2_19; E9=$(( S9+3600 )); adv $(( S9-25*3600 ))
Q9=$(api POST /api/v1/quotes "" "$(jq -nc --arg s "$(iso $S9)" --arg e "$(iso $E9)" '{venue_id:1,member_id:3,start_time:$s,end_time:$e}')")
check "金卡工作日高峰场地费原价 10000" 10000 "$(echo "$Q9" | jq -r .court_fee_cents)"
check "金卡 8 折折后场地费 8000" 8000 "$(echo "$Q9" | jq -r '.court_fee_cents-.discount_cents')"
B9=$(mk "$GOLD" 4 "$S9" "$E9" | bid); pay $B9 "$GOLD"
B9J=$(api GET /api/v1/bookings/$B9 "$GOLD")
check "金卡免费时长抵扣 60 分钟" 60 "$(echo "$B9J" | jq -r .benefit_minutes)"
check "抵扣后应付 0 分" 0 "$(echo "$B9J" | jq -r .payable_cents)"
api POST /api/v1/bookings/$B9/cancel "$GOLD" '{"reason":"场景验证清理"}' >/dev/null

echo "================ 场景 10：高峰退订规则（>=24h 退 80%）+ 幂等 ================"
adv $(( T_D2_19-25*3600 ))   # 距开场 25 小时
E10=$(( T_D2_19+3600 ))
KEY="idem-$(date +%s%N)"
R1=$(mk "$GOLD" 5 "$T_D2_19" "$E10" "{\"idempotency_key\":\"$KEY\",\"use_benefit\":false}")
R2=$(mk "$GOLD" 5 "$T_D2_19" "$E10" "{\"idempotency_key\":\"$KEY\",\"use_benefit\":false}")
check "幂等重试返回同一订单" "$(echo "$R1" | jq -r .booking.id)" "$(echo "$R2" | jq -r .booking.id)"
B10=$(echo "$R1" | bid); PAID10=$(echo "$R1" | jq -r .booking.payable_cents); pay $B10 "$GOLD"
check "高峰提前>=24h 退款比例 80%" 80 "$(api POST /api/v1/bookings/$B10/quote-cancel "$GOLD" | jq -r .refund_percent)"
CAN=$(api POST /api/v1/bookings/$B10/cancel "$GOLD" '{"reason":"临时有事"}')
check "实退金额=80%" $(( PAID10*80/100 )) "$(echo "$CAN" | jq -r .booking.refunded_cents)"
check "订单 cancelled" cancelled "$(echo "$CAN" | jq -r .booking.status)"

echo "================ 场景 11：普通会员抢 24h 外高峰被会员优先规则拒绝 ================"
check "普通会员超优先窗口 -> MEMBER_ONLY_PEAK" MEMBER_ONLY_PEAK \
  "$(mk "$T_GATE" 6 "$T_D2_19" "$E10" | jq -r '.error.code // empty')"

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


echo "================ 场景 15：维护锁定 -> 换场同价场地 -> 闸机灯控同步 -> 维修完工/前台验收后重开 ================"
S15=$(hm 21); E15=$(( S15+3600 )); adv $(( S15-30*60 ))
B15=$(mk "$T_MV" 3 "$S15" "$E15" | bid); pay $B15 "$T_MV"
M15=$(api POST /api/v1/operator/maintenances "$OPS" "$(jq -nc --arg s "$(iso $S15)" --arg e "$(iso $E15)" '{venue_id:1,court_id:3,start_time:$s,end_time:$e,reason:"地胶起翘"}')" | jq -r .id)
check "受影响已支付订单 -> relocation_pending" relocation_pending "$(api GET /api/v1/bookings/$B15 "$T_MV" | jq -r .status)"
truthy "待换场订单 note 写入维护原因" "$(api GET /api/v1/bookings/$B15 "$T_MV" | jq '.note|contains("地胶起翘")')"
OPT15=$(api GET /api/v1/bookings/$B15/relocation/options "$T_MV")
truthy "候选含同时段空闲 4 号场" "$(echo "$OPT15" | jq '[.options[]|select(.court.id==4)]|length==1')"
check "同价场地差价=0" 0 "$(echo "$OPT15" | jq -r '[.options[]|select(.court.id==4)][0].price_diff_cents')"
check "维护时段新预约被拒 MAINTENANCE" MAINTENANCE "$(mk "$T_MV" 3 "$S15" "$E15" | jq -r '.error.code // empty')"
RES15=$(api POST /api/v1/bookings/$B15/relocation/resolve "$T_MV" '{"choice":"move","court_id":4}')
N15=$(echo "$RES15" | jq -r .new_booking.id)
check "旧订单 -> moved" moved "$(api GET /api/v1/bookings/$B15 "$T_MV" | jq -r .status)"
check "旧订单 verdict=moved" moved "$(api GET /api/v1/bookings/$B15 "$T_MV" | jq -r .verdict)"
check "新订单在 4 号场 paid" paid "$(api GET /api/v1/bookings/$N15 "$T_MV" | jq -r .status)"
check "新订单 court_id=4" 4 "$(api GET /api/v1/bookings/$N15 "$T_MV" | jq -r .court_id)"
check "新订单关联 original_court_id=3" 3 "$(api GET /api/v1/bookings/$N15 "$T_MV" | jq -r .original_court_id)"
check "同价换场场馆承担差价=0" 0 "$(echo "$RES15" | jq -r .venue_borne_cents)"
check "权限同步记录 4 条（旧场 revoke*2 + 新场 grant*2）" 4 "$(echo "$RES15" | jq '.device_sync|length')"
OLDCODE=$(api GET /api/v1/bookings/$B15 "$T_MV" | jq -r .entry_code)
NEWCODE=$(api GET /api/v1/bookings/$N15 "$T_MV" | jq -r .entry_code)
adv $(( S15-10*60 ))
check "旧入场码扫码被拒（引导换新码）" denied "$(api POST /api/v1/devices/gate/scan "" "$(jq -nc --arg c "$OLDCODE" '{entry_code:$c,device_id:"G3"}')" | jq -r .result)"
check "新入场码闸机放行新场地" allowed "$(api POST /api/v1/devices/gate/scan "" "$(jq -nc --arg c "$NEWCODE" '{entry_code:$c,device_id:"G4"}')" | jq -r .result)"
TL15=$(api GET /api/v1/bookings/$B15/timeline "$CS")
truthy "timeline 含旧场 revoke 权限同步 2 条" "$(echo "$TL15" | jq '[.device_permission_sync[]|select(.permission=="revoke")]|length==2')"
check "timeline 能看到换入的新订单 id" "$N15" "$(echo "$TL15" | jq -r '.relocated_to.id')"
check "维修完工上报 -> repair_done" repair_done "$(api POST /api/v1/operator/maintenances/$M15/repair-complete "$OPS" '{}' | jq -r .status)"
check "待验收期间场地仍不可订 MAINTENANCE" MAINTENANCE "$(mk "$T_MV" 3 "$S15" "$E15" | jq -r '.error.code // empty')"
check "前台验收不通过 -> 退回 active" active "$(api POST /api/v1/operator/maintenances/$M15/verify "$FRONT" '{"passed":false,"note":"地胶仍有空鼓"}' | jq -r .status)"
api POST /api/v1/operator/maintenances/$M15/repair-complete "$OPS" '{}' >/dev/null
check "前台验收通过 -> verified" verified "$(api POST /api/v1/operator/maintenances/$M15/verify "$FRONT" '{"passed":true,"note":"复检合格"}' | jq -r .status)"
adv $(( S15-5*60 ))
truthy "验收后场地恢复可预约（报价正常返回）" "$(api POST /api/v1/quotes "" "$(jq -nc --arg s "$(iso $S15)" --arg e "$(iso $E15)" '{venue_id:1,court_id:3,member_id:9,start_time:$s,end_time:$e}')" | jq -e '.payable_cents>=0')"
adv $(( S15+25*60 )); job_judge
check "爽约裁决不把 moved 旧单判爽约" moved "$(api GET /api/v1/bookings/$B15 "$T_MV" | jq -r .status)"
check "新单有闸机到场 -> verdict=on_time" on_time "$(api GET /api/v1/bookings/$N15/verdict "$FRONT" | jq -r .verdict)"

echo "================ 场景 16：已扫码进入旧场地 -> 换至高价场（差价场馆承担）+ 清场提醒防误判占场 ================"
api POST /api/v1/operator/courts/5/price "$OPS" '{"price_cents_per_hour":12000}' >/dev/null
check "5 号场专属价生效 12000" 12000 "$(api GET /api/v1/venues/1/courts "" | jq -r '[.courts[]|select(.id==5)][0].price_cents_per_hour')"
S16=$(date -u -d "$D2 08:00" +%s); E16=$(( S16+3600 )); adv $(( S16-10*60 ))
B16=$(mk "$T_MV" 2 "$S16" "$E16" | bid); pay $B16 "$T_MV"
CODE16=$(api GET /api/v1/bookings/$B16 "$T_MV" | jq -r .entry_code)
check "用户已扫码进入旧场地" allowed "$(api POST /api/v1/devices/gate/scan "" "$(jq -nc --arg c "$CODE16" '{entry_code:$c,device_id:"G2"}')" | jq -r .result)"
M16=$(api POST /api/v1/operator/maintenances "$OPS" "$(jq -nc --arg s "$(iso $S16)" --arg e "$(iso $E16)" '{venue_id:1,court_id:2,start_time:$s,end_time:$e,reason:"地胶起翘紧急封闭"}')" | jq -r .id)
check "订单 -> relocation_pending" relocation_pending "$(api GET /api/v1/bookings/$B16 "$T_MV" | jq -r .status)"
OPT16=$(api GET /api/v1/bookings/$B16/relocation/options "$T_MV")
check "高价 5 号场差价 6000 由场馆承担" 6000 "$(echo "$OPT16" | jq -r '[.options[]|select(.court.id==5)][0].venue_borne_cents')"
RES16=$(api POST /api/v1/bookings/$B16/relocation/resolve "$T_MV" '{"choice":"move","court_id":5}')
N16=$(echo "$RES16" | jq -r .new_booking.id)
check "换场结果 venue_borne_cents=6000" 6000 "$(echo "$RES16" | jq -r .venue_borne_cents)"
check "新单 venue_borne_cents=6000 落库" 6000 "$(api GET /api/v1/bookings/$N16 "$T_MV" | jq -r .venue_borne_cents)"
check "新单按高价场地计价 payable=12000（会员原付 6000 不另收）" 12000 "$(api GET /api/v1/bookings/$N16 "$T_MV" | jq -r .payable_cents)"
TL16=$(api GET /api/v1/bookings/$N16/timeline "$CS")
truthy "新单含场馆承担差价补偿支付 6000" "$(echo "$TL16" | jq '[.payments[]|select(.kind=="compensation" and .method=="venue_borne")]|map(.amount_cents)|add==6000')"
truthy "新单含原付款结转 6000" "$(echo "$TL16" | jq '[.payments[]|select(.method=="transferred")]|map(.amount_cents)|add==6000')"
truthy "旧场地清场提醒已生成（pending）" "$(api GET /api/v1/front/vacate-reminders?status=pending "$FRONT" | jq --argjson b "$B16" '[.vacate_reminders[]|select(.booking_id==$b)]|length==1')"
VR16=$(api GET /api/v1/front/vacate-reminders?status=pending "$FRONT" | jq -r --argjson b "$B16" '[.vacate_reminders[]|select(.booking_id==$b)][0].id')
truthy "清场提醒内容含不计占场提示" "$(api GET /api/v1/front/vacate-reminders?status=pending "$FRONT" | jq --argjson id "$VR16" '[.vacate_reminders[]|select(.id==$id)][0].message|contains("不计占场")')"
truthy "旧单记录 vacate_reminded_at" "$(api GET /api/v1/bookings/$B16 "$T_MV" | jq '.vacate_reminded_at!=null')"
job_judge
check "裁决不把已换场用户误判占场/爽约（旧单 moved）" moved "$(api GET /api/v1/bookings/$B16 "$T_MV" | jq -r .status)"
check "新单迁移到场证据 -> on_time" on_time "$(api GET /api/v1/bookings/$N16/verdict "$FRONT" | jq -r .verdict)"
api POST /api/v1/front/vacate-reminders/$VR16/clear "$FRONT" >/dev/null
check "前台确认清场后提醒 -> cleared" cleared "$(api GET /api/v1/front/vacate-reminders?status=cleared "$FRONT" | jq -r --argjson id "$VR16" '[.vacate_reminders[]|select(.id==$id)][0].status')"
api POST /api/v1/operator/maintenances/$M16/repair-complete "$OPS" '{}' >/dev/null
api POST /api/v1/operator/maintenances/$M16/verify "$FRONT" '{"passed":true,"note":"修复验收"}' >/dev/null

echo "================ 场景 17：顺延到其他时间/场地 ================"
S17=$(date -u -d "$D2 11:00" +%s); E17=$(( S17+3600 )); adv $(( S17-30*60 ))
B17=$(mk "$T_MV" 6 "$S17" "$E17" | bid); pay $B17 "$T_MV"
api POST /api/v1/operator/maintenances "$OPS" "$(jq -nc --arg s "$(iso $S17)" --arg e "$(iso $E17)" '{venue_id:1,court_id:6,start_time:$s,end_time:$e,reason:"顶灯故障"}')" >/dev/null
P17S=$(date -u -d "$D2 13:00" +%s); P17E=$(( P17S+3600 ))
OPT17=$(api GET "/api/v1/bookings/$B17/relocation/options?start_time=$(iso $P17S)&end_time=$(iso $P17E)" "$T_MV")
truthy "顺延候选含 13:00 时段 7 号场" "$(echo "$OPT17" | jq '[.options[]|select(.court.id==7)][0].start_time|endswith("13:00:00Z")')"
RES17=$(api POST /api/v1/bookings/$B17/relocation/resolve "$T_MV" "$(jq -nc --arg s "$(iso $P17S)" --arg e "$(iso $P17E)" '{choice:"postpone",court_id:7,start_time:$s,end_time:$e}')")
N17=$(echo "$RES17" | jq -r .new_booking.id)
check "顺延旧单 -> moved" moved "$(api GET /api/v1/bookings/$B17 "$T_MV" | jq -r .status)"
check "顺延新单开始时间=13:00" "$(iso $P17S)" "$(api GET /api/v1/bookings/$N17 "$T_MV" | jq -r .start_time)"
check "顺延新单在 7 号场 paid" paid "$(api GET /api/v1/bookings/$N17 "$T_MV" | jq -r .status)"
adv $(( P17S-10*60 ))
NCODE17=$(api GET /api/v1/bookings/$N17 "$T_MV" | jq -r .entry_code)
check "顺延新入场码闸机放行" allowed "$(api POST /api/v1/devices/gate/scan "" "$(jq -nc --arg c "$NCODE17" '{entry_code:$c,device_id:"G7"}')" | jq -r .result)"

echo "================ 场景 18：用户选择退款 -> 场馆原因全额退款 ================"
S18=$(date -u -d "$D2 16:00" +%s); E18=$(( S18+3600 )); adv $(( S18-30*60 ))
B18=$(mk "$T_MV" 8 "$S18" "$E18" | bid); PAID18=$(api GET /api/v1/bookings/$B18 "$T_MV" | jq -r .payable_cents); pay $B18 "$T_MV"
api POST /api/v1/operator/maintenances "$OPS" "$(jq -nc --arg s "$(iso $S18)" --arg e "$(iso $E18)" '{venue_id:1,court_id:8,start_time:$s,end_time:$e,reason:"地胶起翘"}')" >/dev/null
RES18=$(api POST /api/v1/bookings/$B18/relocation/resolve "$T_MV" '{"choice":"refund","note":"今天来不了了直接退"}')
check "退款选择返回 refund_cents=已付金额" "$PAID18" "$(echo "$RES18" | jq -r .refund_cents)"
B18J=$(api GET /api/v1/bookings/$B18 "$T_MV")
check "订单 -> venue_fault" venue_fault "$(echo "$B18J" | jq -r .status)"
check "全额退款到账" "$PAID18" "$(echo "$B18J" | jq -r .refunded_cents)"
truthy "旧单 note 含维护原因（场馆原因留痕）" "$(echo "$B18J" | jq '.note|contains("地胶起翘")')"
truthy "自动开争议单" "$(api GET /api/v1/cs/disputes?status=open "$CS" | jq --argjson b "$B18" '[.disputes[]|select(.booking_id==$b)]|length==1')"

echo "================ 场景 19：逾期未确认方案 -> 系统按场馆原因自动全额退款 ================"
S19=$(date -u -d "$D2 17:30" +%s); E19=$(( S19+3600 )); adv $(( S19-30*60 ))
B19=$(mk "$T_MV" 1 "$S19" "$E19" | bid); PAID19=$(api GET /api/v1/bookings/$B19 "$T_MV" | jq -r .payable_cents); pay $B19 "$T_MV"
api POST /api/v1/operator/maintenances "$OPS" "$(jq -nc --arg s "$(iso $S19)" --arg e "$(iso $E19)" '{venue_id:1,court_id:1,start_time:$s,end_time:$e,reason:"地胶起翘"}')" >/dev/null
check "未到确认截止不处理（auto_resolved=0）" '{"auto_resolved":0}' "$(job_reloc)"
adv $(( S19-9*60 ))
job_reloc >/tmp/reloc19.out
truthy "超过确认截止 -> 自动处理至少 1 单" "$(jq -e '.auto_resolved>=1' /tmp/reloc19.out)"
B19J=$(api GET /api/v1/bookings/$B19 "$T_MV")
check "目标逾期单 -> venue_fault" venue_fault "$(echo "$B19J" | jq -r .status)"
check "目标逾期单全额退款" "$PAID19" "$(echo "$B19J" | jq -r .refunded_cents)"

echo "================ 场景 20：从高价场换到低价场 -> 差价退还会员；恢复场地原价 ================"
S20=$(date -u -d "$D2 12:00" +%s); E20=$(( S20+3600 )); adv $(( S20-30*60 ))
B20=$(mk "$T_MV" 5 "$S20" "$E20" | bid); PAID20=$(api GET /api/v1/bookings/$B20 "$T_MV" | jq -r .payable_cents); pay $B20 "$T_MV"
check "高价场下单支付 12000（12:00 恒为平峰）" 12000 "$PAID20"
api POST /api/v1/operator/maintenances "$OPS" "$(jq -nc --arg s "$(iso $S20)" --arg e "$(iso $E20)" '{venue_id:1,court_id:5,start_time:$s,end_time:$e,reason:"VIP 场地维护"}')" >/dev/null
RES20=$(api POST /api/v1/bookings/$B20/relocation/resolve "$T_MV" '{"choice":"move","court_id":4}')
N20=$(echo "$RES20" | jq -r .new_booking.id)
check "高价换低价退还差价 6000" 6000 "$(echo "$RES20" | jq -r .refund_cents)"
TL20=$(api GET /api/v1/bookings/$N20/timeline "$CS")
truthy "新单含差价退款支付 6000" "$(echo "$TL20" | jq '[.payments[]|select(.kind=="refund")]|map(.amount_cents)|add==6000')"
check "新单 refunded_cents=6000" 6000 "$(api GET /api/v1/bookings/$N20 "$T_MV" | jq -r .refunded_cents)"
api POST /api/v1/operator/courts/5/price "$OPS" '{"price_cents_per_hour":0}' >/dev/null
check "5 号场恢复场馆基础价 0" 0 "$(api GET /api/v1/venues/1/courts "" | jq -r '[.courts[]|select(.id==5)][0].price_cents_per_hour')"

advReal 0
echo; echo "================================ 结果汇总 ================================"
echo "PASS=$PASS  FAIL=$FAIL"
[ $FAIL -eq 0 ]
