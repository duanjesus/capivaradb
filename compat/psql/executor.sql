-- Milestone 7: the executor.
-- Checked against executor.out by scripts/psql-smoke.sh.

create table customer (id int primary key, name text not null, city text);
create table orders (id int primary key, customer_id int, total int not null);

insert into customer values (1, 'Ana', 'Recife'), (2, 'Bia', 'Recife'), (3, 'Caio', 'Natal'), (4, 'Duda', 'Olinda');
insert into orders values (10, 1, 50), (11, 1, 70), (12, 2, 20), (13, 3, 90), (14, null, 15), (15, 9, 35);

-- Set operations.
select city from customer union select 'Caruaru' order by 1;
select customer_id from orders intersect select id from customer order by 1;
select id from customer except select customer_id from orders;
select customer_id from orders where total > 30 union all select customer_id from orders where total < 60 order by 1 limit 4;

-- The joins that keep unmatched rows of the right side, or of both.
select o.id, c.name from orders o right join customer c on c.id = o.customer_id order by c.name, o.id;
select o.id, c.name from orders o full join customer c on c.id = o.customer_id order by o.id, c.name;

-- USING: the column appears once.
create table city (city text, state text);
insert into city values ('Recife', 'PE'), ('Natal', 'RN'), ('Maceió', 'AL');
select * from customer join city using (city) order by id;

-- Enough rows for the choice of join to matter: 6 144 orders, 3 075 customers.
insert into orders select id + 100, customer_id + 4, total from orders;
insert into orders select id + 200, customer_id + 8, total from orders;
insert into orders select id + 400, customer_id + 16, total from orders;
insert into orders select id + 800, customer_id + 32, total from orders;
insert into orders select id + 1600, customer_id + 64, total from orders;
insert into orders select id + 3200, customer_id + 128, total from orders;
insert into orders select id + 6400, customer_id + 256, total from orders;
insert into orders select id + 12800, customer_id + 512, total from orders;
insert into orders select id + 25600, customer_id + 1024, total from orders;
insert into orders select id + 51200, customer_id + 2048, total from orders;
insert into customer select g.customer_id, 'Cliente ' || g.customer_id, 'Olinda'
from (select distinct customer_id from orders where customer_id > 4) g
where not exists (select 1 from customer c where c.id = g.customer_id);
analyze;

-- No index on orders.customer_id: the customers are hashed and the orders
-- go past once.
explain (costs off) select count(*) from orders o join customer c on c.id = o.customer_id;
select count(*) from orders o join customer c on c.id = o.customer_id;

-- Two tables joined on their primary keys are both already in key order:
-- a merge needs no hash table and no sort.
explain (costs off) select count(*) from orders o join customer c on c.id = o.id;

-- What each step did. The hash table fitted in memory; the sort kept only
-- the three rows it was going to return.
explain (analyze, costs off, timing off)
select c.name, o.total from orders o join customer c on c.id = o.customer_id
order by o.total desc, c.name limit 3;

-- The same with almost no memory: the hash join is done in batches on disk
-- and the sort (now of everything) in runs that are merged.
set work_mem = '64kB';
explain (analyze, costs off, timing off)
select c.name, o.total from orders o join customer c on c.id = o.customer_id
order by o.total desc, c.name;
select c.name, o.total from orders o join customer c on c.id = o.customer_id
order by o.total desc, c.name limit 3;
set work_mem = '4MB';

-- A query that wants three rows reads three rows.
explain (analyze, costs off, timing off) select id from orders limit 3;

-- And the second half of a UNION ALL is not even started if the first half
-- was enough.
explain (analyze, costs off, timing off)
select id from orders union all select id from customer limit 3;

drop table orders;
drop table customer;
drop table city;
