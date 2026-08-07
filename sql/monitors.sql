-- name: CreateMonitorLog :one
insert into monitor_log(
    started_at, ended_at, response_code, error_message, attempts, result, monitor_id
) values(?, ?, ?, ?, ?, ?, ?) returning id;

-- name: CreateMonitorLogLastChecked :exec
insert into monitor_log_last_checked(checked_at, monitor_id, monitor_log_id)
values(?, ?, ?)
on conflict(monitor_id) do update set
    checked_at = excluded.checked_at,
    monitor_log_id = excluded.monitor_log_id;

-- name: GetMonitorLogLastChecked :one
select
    monitor_log.monitor_id, checked_at, monitor_log.response_code
from
    monitor_log_last_checked
    left join monitor_log on monitor_log.id = monitor_log_last_checked.monitor_log_id
where
    monitor_log_last_checked.monitor_id = ?;

-- name: ListAllMonitorLogLastChecked :many
select
    monitor_log.monitor_id, checked_at, monitor_log.response_code
from
    monitor_log_last_checked
    left join monitor_log on monitor_log.id = monitor_log_last_checked.monitor_log_id;

-- name: ListMonitors :many
select
    id, slug, name, url, method, frequency, timeout, attempts,
    request_headers, body_format, body
from
    monitor;

-- name: GetMonitorByID :one
select
    id, name, url, method, frequency, timeout, attempts,
    request_headers, body_format, body
from
    monitor
where
    id = ?;

-- name: EditMonitor :exec
update monitor set
    name = ?, url = ?, method = ?, frequency = ?, timeout = ?, attempts = ?,
    request_headers = ?, body_format = ?, body = ?
where
    id = ?;

-- name: UpdateMonitorSlug :one
update monitor set slug = ? where slug = ? returning id;

-- name: DeleteMonitorByID :exec
delete from monitor where id = ?;

-- name: CreateMonitor :one
insert into monitor(
    slug, name, url, method, frequency, timeout, attempts,
    request_headers, body_format, body
) values(?, ?, ?, ?, ?, ?, ?, ?, ?, ?) returning id;

-- name: DeleteMonitorNotificationChannels :exec
delete from monitor_notification_channel where monitor_id = ?;

-- name: CreateMonitorNotificationChannel :exec
insert into monitor_notification_channel(monitor_id, notification_channel_id)
values(?, ?);

-- name: ListNotificationChannelsByMonitorID :many
select
    notification_channel.id, notification_channel.slug,
    notification_channel.name, notification_channel.type,
    notification_channel.details
from
    monitor_notification_channel
    left join notification_channel
        on notification_channel.id = monitor_notification_channel.notification_channel_id
where
    monitor_notification_channel.monitor_id = ?;

-- name: DeleteMailGroupMonitors :exec
delete from mail_group_monitor where monitor_id = ?;

-- name: CreateMailGroupMonitor :exec
insert into mail_group_monitor(monitor_id, mail_group_id) values(?, ?);

-- name: ListMailGroupIDsByMonitorID :many
select
    mail_group_id, slug
from
    mail_group_monitor
    left join mail_group on mail_group.id = mail_group_id
where
    monitor_id = ?;

-- name: ListMailGroupMembersEmailsByMonitorID :many
select distinct
    email_address
from
    mail_group_member
    left join mail_group on mail_group.id = mail_group_member.mail_group_id
    left join mail_group_monitor on mail_group_monitor.mail_group_id = mail_group.id
where
    mail_group_monitor.monitor_id = ?;

-- name: GetSeverity :one
select severity from severity limit 1;

-- name: UpdateSeverity :exec
update severity set severity = ?;
