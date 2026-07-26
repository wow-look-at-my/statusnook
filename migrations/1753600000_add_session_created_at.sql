alter table session add column created_at datetime;

update session set created_at = strftime('%Y-%m-%d %H:%M:%S', 'now') where created_at is null;
