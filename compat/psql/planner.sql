-- Milestone 6: the planner, seen through EXPLAIN.
-- Checked against planner.out by scripts/psql-smoke.sh.

create table customer (id int primary key, name text not null unique, city text);
create table orders (id int primary key, customer_id int not null, total int not null);
create index orders_customer on orders (customer_id);

insert into customer values (1, 'Ana', 'Recife'), (2, 'Bia', 'Recife'), (3, 'Caio', 'Natal'), (4, 'Duda', 'Natal');
insert into orders values
    (10, 1, 50), (11, 1, 70), (12, 2, 20), (13, 2, 90), (14, 2, 10),
    (15, 3, 35), (16, 3, 60), (17, 1, 15), (18, 4, 80), (19, 2, 45),
    (20, 1, 25), (21, 3, 55), (22, 4, 65), (23, 2, 30), (24, 1, 95);
-- Enough rows, and enough different customers, for an index to be worth
-- using: 15 orders become 480, 4 customers become 128.
insert into orders select id + 100, customer_id + 10, total from orders;
insert into orders select id + 200, customer_id + 20, total from orders;
insert into orders select id + 400, customer_id + 40, total from orders;
insert into orders select id + 800, customer_id + 80, total from orders;
insert into orders select id + 1600, customer_id + 160, total from orders;
insert into customer select distinct customer_id, 'Cliente ' || customer_id, 'Olinda' from orders where customer_id > 4;
analyze;

-- The primary key finds one row without reading the table.
explain select * from orders where id = 17;

-- A secondary index; the condition it cannot use stays as a filter.
explain select * from orders where customer_id = 2 and total > 40;

-- Nothing to go on: a sequential scan.
explain select * from orders where total > 40;

-- A join written orders-first. The planner starts from the one customer
-- asked for, found through the unique index on its name, and looks its
-- orders up through the index on customer_id.
explain select c.name, o.total
from orders o join customer c on c.id = o.customer_id
where c.name = 'Bia';

-- The same query with the planner's switches off: the plan it rejected.
set enable_indexscan = off;
set join_collapse_limit = 1;
explain select c.name, o.total
from orders o join customer c on c.id = o.customer_id
where c.name = 'Bia';
set enable_indexscan = on;
set join_collapse_limit = 8;

-- EXPLAIN ANALYZE runs the query and shows what each step really did.
explain (analyze, timing off)
select c.city, count(*), sum(o.total)
from customer c left join orders o on o.customer_id = c.id
where c.city = 'Recife'
group by c.city
order by 2 desc;

-- Updates and deletes find their rows the same way.
explain (costs off) update orders set total = total + 1 where id = 10;
explain (costs off) delete from orders where customer_id = 2;
