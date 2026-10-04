-- 0002: 场地维护临时换场
-- 1) 场地可选差异化定价（如 VIP 场地）；null/0 表示沿用场馆基础价
alter table courts add column if not exists price_cents_per_hour bigint;

-- 2) 预约单增加换场链路字段
alter table bookings add column if not exists relocation_status text not null default ''; -- '' / pending / resolved
alter table bookings add column if not exists relocation_maintenance_id bigint references maintenance_windows(id);
alter table bookings add column if not exists original_court_id bigint references courts(id);
alter table bookings add column if not exists relocated_from_booking_id bigint references bookings(id);
alter table bookings add column if not exists relocated_to_booking_id bigint references bookings(id);
alter table bookings add column if not exists relocation_due_at timestamptz;
alter table bookings add column if not exists venue_borne_cents bigint not null default 0; -- 换至更贵场地由场馆承担的差价
alter table bookings add column if not exists vacate_reminded_at timestamptz;

alter table bookings drop constraint if exists bookings_status_check;
alter table bookings add constraint bookings_status_check check (status in (
  'held','paid','warming','in_use','completed','cancelled','expired',
  'no_show','venue_fault','disputed','relocation_pending','moved'));

alter table bookings drop constraint if exists bookings_verdict_check;
alter table bookings add constraint bookings_verdict_check check (verdict is null or verdict in
  ('on_time','late','no_show','venue_fault','ambiguous','excused','moved'));

-- 3) 维护单增加“维修完成 -> 前台验收”流转
alter table maintenance_windows add column if not exists repair_finished_at timestamptz;
alter table maintenance_windows add column if not exists verified_by bigint references staff(id);
alter table maintenance_windows add column if not exists verified_at timestamptz;
alter table maintenance_windows add column if not exists verify_note text not null default '';

alter table maintenance_windows drop constraint if exists maintenance_windows_status_check;
alter table maintenance_windows add constraint maintenance_windows_status_check
  check (status in ('active','released','repair_done','verified'));

-- 4) 闸机 / 灯控权限同步留痕（换场确认后旧场地 revoke、新场地 grant）
create table if not exists device_permission_sync (
  id           bigserial primary key,
  booking_id   bigint not null references bookings(id),
  court_id     bigint not null references courts(id),
  entry_code   text not null default '',
  device_type  text not null check (device_type in ('gate','light')),
  permission   text not null check (permission in ('grant','revoke')),
  reason       text not null default '',
  sync_status  text not null default 'synced' check (sync_status in ('synced','failed')),
  created_by   bigint,
  created_at   timestamptz not null default now()
);
create index if not exists idx_device_sync_booking on device_permission_sync(booking_id);

-- 5) 旧场地清场提醒（用户已扫码进入被维护场地时生成，防止被误判为用户占场）
create table if not exists vacate_reminders (
  id             bigserial primary key,
  booking_id     bigint not null references bookings(id),
  venue_id       bigint not null references venues(id),
  court_id       bigint not null references courts(id),
  maintenance_id bigint references maintenance_windows(id),
  message        text not null default '',
  status         text not null default 'pending' check (status in ('pending','cleared')),
  cleared_by     bigint,
  reminded_at    timestamptz not null default now(),
  cleared_at     timestamptz,
  created_at     timestamptz not null default now()
);
create index if not exists idx_vacate_status on vacate_reminders(status, reminded_at);
