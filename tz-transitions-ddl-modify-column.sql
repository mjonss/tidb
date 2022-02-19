-- test for PR 31843 / issue 29585
-- TIMESTAMP to bigint
drop table if exists t;
set @@session.time_zone='+00:00';
create table t(id int primary key auto_increment, c1 timestamp, index idx(c1));
-- DST change in spring skipping one hour, no issues timestamp -> bigint, but reverse if bigint falls in between non-exising hour
insert into t values (NULL, "2016-03-13 09:30:00"), (NULL, "2016-03-13 10:30:00");
-- DST change in autumn repeating one hour, non-deterministic bigint -> timestamp for the repeating hour, is it with or without DST?
insert into t values (NULL, "2016-11-06 08:30:00"), (NULL, "2016-11-06 09:30:00"), (NULL, "2016-11-06 10:30:00");
select id, c1, unix_timestamp(c1), unix_timestamp(c1)/3600 from t;
set @@session.time_zone='America/Los_Angeles';
-- select id, c1, CAST(c1 AT TIME ZONE 'UTC' AS DATETIME) as 'c1_UTC', unix_timestamp(c1), unix_timestamp(c1)/3600 from t;
select id, c1, CONVERT_TZ(c1, 'America/Los_Angeles', 'UTC') as c1_UTC, unix_timestamp(c1), unix_timestamp(c1)/3600 from t;
alter table t modify column c1 bigint;
select * from t;
alter table t add index idx1(id, c1);
select * from t;

-- bigint to TIMESTAMP, with sql_mode = '' (silently truncate/convert non-existing timestamps)
drop table if exists t;
create table t(id int primary key auto_increment, c1 bigint, index idx(c1));
-- DST change in spring skipping one hour, no issues timestamp -> bigint, but reverse if bigint falls in between non-exising hour
insert into t values (NULL, "20160313013000"), (NULL, "20160313023000"), (NULL, "20160313033000");
-- DST change in autumn repeating one hour, non-deterministic bigint -> timestamp for the repeating hour, is it with or without DST?
insert into t values (NULL, "20161106013000"), (NULL, "20161106023000"), (NULL, "20161106033000");
select * from t;
set @@session.time_zone='America/Los_Angeles';
set @@sql_mode='';
alter table t modify column c1 timestamp;
-- select id, c1, CAST(c1 AT TIME ZONE 'UTC' AS DATETIME) as 'c1_UTC', unix_timestamp(c1), unix_timestamp(c1)/3600 from t;
select id, c1, CONVERT_TZ(c1, 'America/Los_Angeles', 'UTC') as c1_UTC, unix_timestamp(c1), unix_timestamp(c1)/3600 from t;
alter table t add index idx1(id, c1);
select * from t;
set @@session.time_zone='+00:00';
select * from t;

-- bigint to TIMESTAMP, with sql_mode = DEFAULT (NOT silently truncate/convert non-existing timestamps)
drop table if exists t;
create table t(id int primary key auto_increment, c1 bigint, index idx(c1));
-- DST change in spring skipping one hour, no issues timestamp -> bigint, but reverse if bigint falls in between non-exising hour
insert into t values (NULL, "20160313013000"), (NULL, "20160313023000"), (NULL, "20160313033000");
-- DST change in autumn repeating one hour, non-deterministic bigint -> timestamp for the repeating hour, is it with or without DST?
insert into t values (NULL, "20161106013000"), (NULL, "20161106023000"), (NULL, "20161106033000");
select * from t;
set @@session.time_zone='America/Los_Angeles';
set @@sql_mode=DEFAULT;
-- Expect error: ERROR 1292 (22007): Incorrect datetime value: '20160313023000' for column 'c1' at row 2
alter table t modify column c1 timestamp;
delete from t where id = 2;
alter table t modify column c1 timestamp;
select * from t;
-- select id, c1, CAST(c1 AT TIME ZONE 'UTC' AS DATETIME) as 'c1_UTC', unix_timestamp(c1), unix_timestamp(c1)/3600 from t;
select id, c1, CONVERT_TZ(c1, 'America/Los_Angeles', 'UTC') as c1_UTC, unix_timestamp(c1), unix_timestamp(c1)/3600 from t;
alter table t add index idx1(id, c1);
select * from t;
set @@session.time_zone='+00:00';
select * from t;

select version();
