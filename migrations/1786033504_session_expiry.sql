-- Sessions had no timestamp at all and were only deleted on logout or on a
-- password change, while the cookie was set to expire in 100 years. A token
-- lifted from a backup or a stale browser profile stayed a valid admin
-- credential forever, and the table grew a row per login.
--
-- Existing sessions are backfilled to now rather than to null, so nobody is
-- logged out by the upgrade itself.
create table session_temp(
    id integer primary key,
    token text not null unique,
    csrf_token text not null unique,
    created_at datetime not null,
    user_id int references user(id) on delete cascade not null
);

insert into
    session_temp(id, token, csrf_token, created_at, user_id)
select
    id, token, csrf_token, datetime('now'), user_id
from
    session;

drop table session;

alter table
    session_temp rename to session;

create index idx_session_user_id on session(user_id);

create index idx_session_created_at on session(created_at);
