-- A psql session exercising the simple query protocol end to end.
-- scripts/psql-smoke.sh feeds this file to psql and compares the output
-- with expected.out.

select version();

create table capivaras (
    id      int primary key,
    name    text not null,
    weight  double precision,
    wild    boolean
);

insert into capivaras values
    (1, 'Filó',    48.5, true),
    (2, 'Bolota',  61.0, false),
    (3, 'Pingo',   null, true);

select * from capivaras;

select name, weight * 2 as double_weight, weight is null as unknown_weight
from capivaras
where wild;

-- Several statements in one message, as psql sends them with \;
select 1 as one \; select 'two' as two;

update capivaras set weight = 35.2 where name = 'Pingo';
delete from capivaras where not wild;
select id, name, weight from capivaras;

-- Transactions: the prompt-less psql still tracks the status byte.
begin;
insert into capivaras values (4, 'Temporária', 1, true);
select name from capivaras where id = 4;
rollback;
select name from capivaras where id = 4;

-- Errors carry a position, which psql turns into a caret.
select nome from capivaras;
selec 1;
insert into capivaras values (1, 'Repetida', 1, true);
select 1 / 0;

-- A failed transaction refuses work until it is rolled back.
begin;
select 1 / 0;
select 1;
commit;

show transaction isolation level;
