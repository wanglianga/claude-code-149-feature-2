-- 0002: 场地维护临时换场 / 维护验收流 / 场地级价格 / 前台通知
-- 依赖 0001_init.sql。金额单位：分（bigint），时间：timestamptz

-- ============ 场地：单场地价格覆盖（为空则用场馆统一价） ============
alter table courts add column if not exists price_cents_per_hour bigint;

-- ============ 预约：换场终态 + 维护原因 + 继任订单 ============
alter table bookings drop constraint if exists bookings_status_check;
alter table bookings add constraint bookings_status_check
  check (status in (
    'held','paid','warming','in_use',
    'completed','cancelled','expired','no_show','venue_fault','disputed','relocated'));
alter table bookings add column if not exists maintenance_reason text not null default '';
alter table bookings add column if not exists superseded_by bigint references bookings(id);
-- relocated 为终态，不在 GiST 排他约束的活跃状态集合内，不会继续占位

-- ============ 维护窗口：完工 -> 前台验收 -> 才重新开放 ============
alter table maintenance_windows drop constraint if exists maintenance_windows_status_check;
alter table maintenance_windows add constraint maintenance_windows_status_check
  check (status in ('active','acceptance_pending','released'));
alter table maintenance_windows add column if not exists finished_by bigint;
alter table maintenance_windows add column if not exists finished_at timestamptz;
alter table maintenance_windows add column if not exists accepted_by bigint;
alter table maintenance_windows add column if not exists accepted_at timestamptz;
alter table maintenance_windows add column if not exists acceptance_note text not null default '';

-- ============ 换场方案（维护锁定时为受影响订单生成） ============
create table if not exists relocation_offers (
  id                   bigserial primary key,
  booking_id           bigint not null references bookings(id),
  maintenance_id       bigint not null references maintenance_windows(id),
  venue_id             bigint not null references venues(id),
  old_court_id         bigint not null references courts(id),
  status               text not null default 'pending' check (status in
                         ('pending','accepted_relocate','accepted_postpone','refunded','expired')),
  candidates           jsonb not null default '[]'::jsonb, -- 同时间可用场地快照（含差价）
  clearance_needed     boolean not null default false,     -- 用户已扫码进旧场地，需清场
  resolution           jsonb,                              -- 用户确认结果快照
  new_booking_id       bigint references bookings(id),
  venue_subsidy_cents  bigint not null default 0,          -- 换到更高价场地时场馆承担的差价
  reason               text not null default '',           -- 维护原因（同步写入旧订单）
  created_by           bigint,
  decided_by           bigint,
  decided_at           timestamptz,
  created_at           timestamptz not null default now()
);
create index if not exists idx_relocation_booking on relocation_offers(booking_id);
create index if not exists idx_relocation_pending on relocation_offers(status) where status='pending';

-- ============ 前台通知（旧场地清场提醒等） ============
create table if not exists ops_notifications (
  id          bigserial primary key,
  venue_id    bigint not null references venues(id),
  court_id    bigint,
  booking_id  bigint,
  type        text not null check (type in ('clearance_reminder')),
  message     text not null,
  status      text not null default 'pending' check (status in ('pending','done')),
  created_at  timestamptz not null default now(),
  acked_by    bigint,
  acked_at    timestamptz
);
create index if not exists idx_ops_notifications_status on ops_notifications(status) where status='pending';
