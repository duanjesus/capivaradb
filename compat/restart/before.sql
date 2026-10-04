-- First server process: create and fill a database on disk.
create table capivaras (
    id      int primary key,
    name    text not null unique,
    weight  double precision default 45.5,
    wild    boolean default true
);
create index capivaras_weight on capivaras (weight);

insert into capivaras (id, name, weight) values (1, 'Filó', 48.5), (2, 'Bolota', 61), (3, 'Pingo', 35.2);
insert into capivaras (id, name) values (4, 'Padrão');
update capivaras set wild = false where name = 'Bolota';
delete from capivaras where id = 3;

select * from capivaras;

-- A transaction left open: the server is killed while it is in progress.
begin;
insert into capivaras (id, name) values (99, 'Fantasma');
update capivaras set weight = 0;
select id, name, weight from capivaras order by id;

-- No COMMIT, no CHECKPOINT, no clean shutdown: the server is killed here.
