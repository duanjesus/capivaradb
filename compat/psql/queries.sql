-- Milestone 2: the SQL the parser and the in-memory executor now handle.
-- Checked against queries.out by scripts/psql-smoke.sh.

create table dept (
    id    int primary key,
    name  text not null unique
);

create table emp (
    id       int primary key,
    name     text not null,
    dept_id  int,
    salary   int default 50,
    boss     int
);

insert into dept values (1, 'engenharia'), (2, 'operações'), (3, 'vazio');
insert into emp values
    (1, 'Ana',   1, 100, null),
    (2, 'Bia',   1,  80, 1),
    (3, 'Caio',  2,  60, 1),
    (4, 'Duda',  2,  60, 3),
    (5, 'Enzo',  null, null, 1);
insert into emp (id, name, dept_id) values (6, 'Fabi', 1);

-- Joins: inner drops Enzo, left keeps the empty department.
select e.name, d.name as dept, b.name as boss
from emp e
join dept d on d.id = e.dept_id
left join emp b on b.id = e.boss
order by e.id;

select d.name, count(e.id) as people, sum(e.salary) as payroll, avg(e.salary) as average
from dept d
left join emp e on e.dept_id = d.id
group by d.name
order by people desc, d.name;

select dept_id, count(*) from emp group by dept_id having count(*) > 1 order by 1;

-- Subqueries: scalar, correlated, EXISTS, IN.
select name, salary from emp where salary = (select max(salary) from emp);
select name from emp e where salary > (select avg(salary) from emp where dept_id = e.dept_id);
select name from dept d where not exists (select 1 from emp e where e.dept_id = d.id);
select s.dept_id, s.total
from (select dept_id, sum(salary) as total from emp group by dept_id) s
where s.total > 150;

-- CASE, IN, BETWEEN, LIKE, DISTINCT, LIMIT.
select name,
       case when salary >= 100 then 'alto' when salary >= 70 then 'médio'
            when salary is null then '?' else 'baixo' end as faixa
from emp
order by salary desc nulls last, name
limit 4;

select distinct salary from emp where salary between 50 and 90 order by salary;
select name from emp where name like '%a' and dept_id in (1, 2) order by name offset 1;

-- Constraints with their PostgreSQL names.
insert into dept values (4, 'vazio');
create unique index emp_name on emp (name);
insert into emp values (7, 'Ana', 1, 1, null);

-- Errors that only a real binder can give.
select name, count(*) from emp;
select id from emp join dept on emp.dept_id = dept.id;
select * from emp where salary = (select salary from emp);
