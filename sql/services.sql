-- name: ListServices :many
select
    id, slug, name, helper_text
from
    service;

-- name: CreateService :exec
insert into service(slug, name, helper_text) values(?, ?, ?);

-- name: DeleteServiceByID :exec
delete from service where id = ?;

-- name: GetServiceByID :one
select id, name, helper_text from service where id = ?;

-- name: EditService :exec
update service set name = ?, helper_text = ? where id = ?;

-- name: UpdateServiceSlug :one
update service set slug = ? where slug = ? returning id;
