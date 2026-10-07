-- Milestone 8: ORDER BY without a sort, and hash tables larger than memory.
-- Checked against ordering.out by scripts/psql-smoke.sh.

create table item (id int primary key, owner int not null, price int not null, label text not null);
create index item_price on item (price);
create index item_owner_price on item (owner, price);

insert into item values (1, 1, 919, 'item 1'), (2, 2, 838, 'item 2'), (3, 3, 757, 'item 3'), (4, 4, 676, 'item 4');
insert into item select id + 4, (id + 4) % 64, ((id + 4) * 7919) % 1000, 'item ' || cast(id + 4 as text) from item;
insert into item select id + 8, (id + 8) % 64, ((id + 8) * 7919) % 1000, 'item ' || cast(id + 8 as text) from item;
insert into item select id + 16, (id + 16) % 64, ((id + 16) * 7919) % 1000, 'item ' || cast(id + 16 as text) from item;
insert into item select id + 32, (id + 32) % 64, ((id + 32) * 7919) % 1000, 'item ' || cast(id + 32 as text) from item;
insert into item select id + 64, (id + 64) % 64, ((id + 64) * 7919) % 1000, 'item ' || cast(id + 64 as text) from item;
insert into item select id + 128, (id + 128) % 64, ((id + 128) * 7919) % 1000, 'item ' || cast(id + 128 as text) from item;
insert into item select id + 256, (id + 256) % 64, ((id + 256) * 7919) % 1000, 'item ' || cast(id + 256 as text) from item;
insert into item select id + 512, (id + 512) % 64, ((id + 512) * 7919) % 1000, 'item ' || cast(id + 512 as text) from item;
insert into item select id + 1024, (id + 1024) % 64, ((id + 1024) * 7919) % 1000, 'item ' || cast(id + 1024 as text) from item;
insert into item select id + 2048, (id + 2048) % 64, ((id + 2048) * 7919) % 1000, 'item ' || cast(id + 2048 as text) from item;
insert into item select id + 4096, (id + 4096) % 64, ((id + 4096) * 7919) % 1000, 'item ' || cast(id + 4096 as text) from item;
analyze;

-- The table is stored in primary key order: nothing to sort, and with a
-- LIMIT nothing to read beyond the rows wanted.
explain (analyze, costs off, timing off) select id, price from item order by id limit 3;

-- An index is in the order of its columns. Three entries are read from it
-- instead of 8 192 rows being sorted.
explain (analyze, costs off, timing off) select id, price from item order by price limit 3;
select id, price from item order by price limit 3;

-- The column an equality fixes does not count: within one owner, the index
-- on (owner, price) is in price order.
explain (analyze, costs off, timing off) select id, price from item where owner = 5 order by price limit 3;

-- Without a LIMIT every row is wanted, and going through a secondary index
-- for each costs more than scanning and sorting.
explain (costs off) select id, price from item order by price;

-- Descending order still sorts (a heap of three rows, but the whole table
-- is read).
explain (analyze, costs off, timing off) select id, price from item order by price desc limit 3;

-- Grouping keeps one entry per group in a hash table. With room for all
-- 8 192 groups it is done in one pass...
explain (analyze, costs off, timing off) select label, count(*) from item group by label;

-- ...and without, the groups that do not fit are set aside on disk and
-- grouped in later passes.
set work_mem = '64kB';
explain (analyze, costs off, timing off) select label, count(*) from item group by label;
select count(*), sum(n) from (select label, count(*) as n from item group by label) g;
explain (analyze, costs off, timing off) select distinct label from item;
explain (analyze, costs off, timing off) select id from item except select id + 1 from item;
select id from item except select id + 1 from item;
set work_mem = '4MB';

drop table item;
