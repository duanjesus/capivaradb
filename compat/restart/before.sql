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

-- Without a write-ahead log, a checkpoint (or a clean shutdown) is what
-- puts the changes on disk. The server is killed right after this.
checkpoint;
