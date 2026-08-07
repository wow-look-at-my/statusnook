-- `token string` is not a SQLite type name, so the column took NUMERIC
-- affinity: an all-digit token would be stored and compared as an integer.
-- Base64 tokens have kept that latent. Rebuilt first, because dropping the
-- old table would drop any index created on it.
create table pending_email_alert_subscription_temp(
    id integer primary key,
    token text not null,
    email text not null collate nocase,
    created_at datetime not null,
    confirmed_at datetime
);

insert into
    pending_email_alert_subscription_temp
select
    *
from
    pending_email_alert_subscription;

drop table pending_email_alert_subscription;

alter table
    pending_email_alert_subscription_temp rename to pending_email_alert_subscription;

-- Every lookup below was a full table scan. Verified with EXPLAIN QUERY PLAN:
-- each one becomes a SEARCH with these indexes.
--
-- mail_group_member(mail_group_id) is deliberately absent: unique(mail_group_id,
-- email_address) already indexes that prefix.

create index idx_alert_notification_unsent
    on alert_notification(sent_at) where sent_at is null;

create index idx_alert_message_alert_id on alert_message(alert_id);

create index idx_alert_service_alert_id on alert_service(alert_id);

create index idx_alert_ongoing on alert(ended_at) where ended_at is null;

create index idx_mail_group_monitor_monitor_id on mail_group_monitor(monitor_id);

create index idx_pending_email_alert_subscription_email
    on pending_email_alert_subscription(email);

create index idx_pending_email_alert_subscription_token
    on pending_email_alert_subscription(token);

create index idx_alert_subscription_destination on alert_subscription(destination);

create index idx_alert_subscription_meta on alert_subscription(meta);

create index idx_session_user_id on session(user_id);
