-- name: UpdateMetaValue :exec
insert into meta(name, value) values(?, ?)
on conflict(name) do update set value = excluded.value;

-- name: GetMetaValue :one
select value from meta where name = ?;
