-- name: ListUnsentAlertNotifications :many
select
    alert_notification.id, alert_subscription.destination, alert_message.content,
    alert_subscription.type, alert_message.id as alert_message_id, alert.title,
    alert.type as alert_type, alert.severity,
    -- char(8226) is the bullet separator, spelled out because a literal
    -- multi-byte character here shifts sqlc's offsets and silently truncates
    -- every query after it in the file.
    -- coalesce because deleting the last service an alert names cascades its
    -- alert_service rows away, and group_concat over no rows is null.
    cast(
        coalesce(group_concat(service.name, ' ' || char(8226) || ' '), '') as text
    ) as alert_services
from alert_notification
left join alert_subscription on alert_subscription.id = alert_subscription_id
left join alert_message on alert_message.id = alert_message_id
left join alert on alert.id = alert_message.alert_id
left join alert_service on alert_service.alert_id = alert_message.alert_id
left join service on service.id = alert_service.service_id
where alert_notification.sent_at is null
group by alert_notification.id
order by alert_message.created_at asc;

-- name: ListActiveAlertEmailSubscriptions :many
select id, type, destination, meta, active from alert_subscription
where type = 'email' and active = true;

-- name: DeleteAlertSubscriptionByMeta :exec
delete from alert_subscription where meta = ?;

-- name: UpdateEmailAlertSubscriptionActiveByMeta :exec
update alert_subscription set active = ? where meta = ? and type = 'email';

-- name: UpdateEmailAlertSubscriptionActiveByEmail :exec
update alert_subscription set active = ? where destination = ? and type = 'email';

-- name: CreateAlertSubscription :exec
insert into alert_subscription(type, destination, meta) values(?, ?, nullif(?, ''))
on conflict(type, destination) do update set active = true;

-- name: CheckHasRecentPendingEmailAlertSubscription :one
select exists(
    select 1 from pending_email_alert_subscription where email = ?
    and created_at > datetime(?, '-10 minutes')
);

-- name: CreatePendingEmailAlertSubscription :exec
insert into pending_email_alert_subscription(token, email, created_at)
values(?, ?, ?);

-- name: UpdatePendingEmailAlertSubscription :exec
update pending_email_alert_subscription set confirmed_at = ? where token = ?;

-- name: GetAlertSubscriptionByEmail :one
select id, type, destination, meta, active from alert_subscription
where type = 'email' and destination = ?;

-- name: GetPendingEmailAlertSubscriptionEmailByToken :one
select email from pending_email_alert_subscription
where token = ? and confirmed_at is null and created_at > ?;

-- name: ListAlerts :many
select id, title, type, severity, created_at, ended_at from alert
order by created_at desc;

-- name: ListOngoingAlerts :many
select id, title, type, severity, created_at, ended_at
from alert
where ended_at is null
order by case
    when severity = 'red' then 1
    when severity = 'amber' then 2
    else 3
end asc;

-- name: ListAlertsInPeriod :many
select id, title, type, severity, created_at, ended_at
from alert
where created_at >= ? and created_at < ?
order by created_at desc;

-- name: GetAlertByID :one
select id, title, type, severity, created_at, ended_at from alert where id = ?;

-- sqlc's sqlite engine has no dynamic IN list, so each view re-expresses its
-- alert set as a subquery instead of interpolating the ids just fetched.

-- name: ListOngoingAlertMessages :many
select id, content, created_at, last_updated_at, alert_id
from alert_message
where alert_id in (select id from alert where ended_at is null)
order by created_at desc;

-- name: ListOngoingAlertServices :many
select service.id, service.name, service.helper_text, alert_id
from alert_service
left join service on service.id = alert_service.service_id
where alert_id in (select id from alert where ended_at is null);

-- name: ListAlertMessagesInPeriod :many
select id, content, created_at, last_updated_at, alert_id
from alert_message
where alert_id in (select id from alert where alert.created_at >= ? and alert.created_at < ?)
order by created_at desc;

-- name: ListAlertServicesInPeriod :many
select service.id, service.name, service.helper_text, alert_id
from alert_service
left join service on service.id = alert_service.service_id
where alert_id in (select id from alert where alert.created_at >= ? and alert.created_at < ?);

-- name: ListAlertMessagesByAlertID :many
select id, content, created_at, last_updated_at, alert_id
from alert_message
where alert_id = ?
order by created_at desc;

-- name: ListAlertServicesByAlertID :many
select service.id, service.name, service.helper_text, alert_id
from alert_service
left join service on service.id = alert_service.service_id
where alert_id = ?;

-- name: GetOldestAlertDate :one
select created_at from alert order by created_at asc limit 1;

-- name: ListAlertSettings :many
select name, value from alert_setting;

-- name: GetAlertSMTPNotificationSetting :one
select notification_channel_id from alert_setting_smtp_notification limit 1;

-- name: DeleteAlertSMTPNotificationSetting :exec
delete from alert_setting_smtp_notification;

-- name: CreateAlertSMTPNotificationSetting :exec
insert into alert_setting_smtp_notification(notification_channel_id) values(?);

-- name: DeleteAlertByID :exec
delete from alert where id = ?;

-- name: CreateAlertMessageNotifications :exec
insert into alert_notification(created_at, alert_subscription_id, alert_message_id)
select ?, id, ? from alert_subscription where alert_subscription.active = true;

-- name: UpdateAlertSentAtByID :exec
update alert_notification set sent_at = ? where id = ?;

-- name: ResolveAlert :exec
update alert set ended_at = ? where id = ?;

-- name: UnresolveAlert :exec
update alert set ended_at = null where id = ?;

-- name: CreateAlert :one
insert into alert(title, type, severity, created_at) values(?, ?, ?, ?) returning id;

-- name: EditAlert :exec
update alert set title = ?, type = ?, severity = ? where id = ?;

-- name: AddAlertService :exec
insert into alert_service(alert_id, service_id) values(?, ?);

-- name: DeleteAlertServicesByAlertID :exec
delete from alert_service where alert_id = ?;

-- name: CreateAlertMessage :one
insert into alert_message(content, created_at, alert_id) values(?, ?, ?) returning id;

-- name: DeleteAlertMessageByID :exec
delete from alert_message where alert_id = ? and id = ?;

-- name: EditAlertMessage :exec
update alert_message set content = ?, last_updated_at = ?
where alert_id = ? and id = ?;

-- name: UpsertAlertSetting :exec
insert into alert_setting(name, value) values(?, ?)
on conflict(name) do update set value = excluded.value;

-- name: PruneAlertNotifications :execrows
delete from alert_notification where id in (
    select id from alert_notification
    where alert_notification.sent_at is not null and alert_notification.sent_at < ? limit ?
);

-- name: PrunePendingEmailAlertSubscriptions :execrows
delete from pending_email_alert_subscription where id in (
    select id from pending_email_alert_subscription
    where pending_email_alert_subscription.created_at < ? limit ?
);
