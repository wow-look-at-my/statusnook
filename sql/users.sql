-- name: ListActiveUserInvitations :many
select id, token, created_at from user_invitation where created_at > ? order by id desc;

-- name: ValidateUserInvitationToken :one
select id from user_invitation where token = ? and created_at > ?;

-- name: CreateUserInvitation :exec
insert into user_invitation(token, created_at) values(?, ?);

-- name: DeleteUserInvitation :exec
delete from user_invitation where id = ?;

-- name: CreateSession :exec
insert into session(token, csrf_token, created_at, user_id) values(?, ?, ?, ?);

-- name: ValidateSession :one
select
    user.id, session.csrf_token
from
    user
    left join session on session.user_id = user.id
where
    session.token = ? and session.created_at > ?;

-- name: ListUsers :many
select id, username from user;

-- name: GetPasswordHash :one
select password, id from user where username = ?;

-- name: GetUsernameByID :one
select username from user where id = ?;

-- name: DeleteSession :exec
delete from session where token = ?;

-- name: DeleteAllSessionsByUserID :exec
delete from session where user_id = ?;

-- name: CreateUser :one
insert into user(username, password) values(?, ?) returning id;

-- name: EditUserUsername :exec
update user set username = ? where id = ?;

-- name: EditUser :exec
update user set username = ?, password = ? where id = ?;

-- name: DeleteUserByID :exec
delete from user where id = ?;

-- name: PruneUserInvitations :execrows
delete from user_invitation where id in (
    select id from user_invitation where user_invitation.created_at < ? limit ?
);

-- name: PruneSessions :execrows
delete from session where id in (
    select id from session where session.created_at < ? limit ?
);
