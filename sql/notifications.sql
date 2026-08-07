-- name: ListNotificationChannels :many
select id, slug, name, type, details from notification_channel
where sqlc.narg('type') is null or type = sqlc.narg('type');

-- name: CreateNotificationChannel :exec
insert into notification_channel(slug, name, type, details) values(?, ?, ?, ?);

-- name: GetNotificationChannelByID :one
select id, slug, name, type, details from notification_channel
where id = ?;

-- name: GetNotificationChannelBySlug :one
select id, slug, name, type, details from notification_channel
where slug = ?;

-- name: EditNotificationChannel :exec
update notification_channel set name = ?, details = ?
where id = ?;

-- name: UpdateNotificationChannelSlug :one
update notification_channel set slug = ? where slug = ? returning id;

-- name: DeleteNotificationChannelByID :exec
delete from notification_channel where id = ?;

-- name: ListMailGroups :many
select id, slug, name, description from mail_group;

-- name: ListMailGroupMembersByID :many
select id, email_address from mail_group_member where mail_group_id = ?;

-- name: GetMailGroupByID :one
select id, name, description from mail_group where id = ?;

-- name: CreateMailGroup :one
insert into mail_group(slug, name, description) values(?, ?, ?) returning id;

-- name: UpdateMailGroup :exec
update mail_group set name = ?, description = ? where id = ?;

-- name: UpdateMailGroupSlug :one
update mail_group set slug = ? where slug = ? returning id;

-- name: DeleteMailGroupMembersByGroupID :exec
delete from mail_group_member where mail_group_id = ?;

-- name: AddMailGroupMember :exec
insert into mail_group_member(email_address, mail_group_id) values(?, ?)
on conflict (mail_group_id, email_address) do nothing;

-- name: DeleteMailGroupByID :exec
delete from mail_group where id = ?;
