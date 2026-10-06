package engine

import (
	"testing"

	"github.com/duanjesus/capivaradb/internal/pgerr"
)

// company loads a small schema with the usual traps: a department without
// employees, an employee without a department, NULL salaries, duplicates.
func company(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, New())
	h.mustRun(`
		create table dept (id int primary key, name text not null);
		create table emp (id int primary key, name text not null, dept_id int, salary int, boss int);
		insert into dept values (1, 'eng'), (2, 'ops'), (3, 'empty');
		insert into emp values
			(1, 'ana',   1, 100, null),
			(2, 'bia',   1, 80,  1),
			(3, 'caio',  2, 60,  1),
			(4, 'duda',  2, 60,  3),
			(5, 'enzo',  null, null, 1),
			(6, 'fabi',  1, null, 2);
	`)
	return h
}

func TestJoins(t *testing.T) {
	h := company(t)
	h.expect("select e.name, d.name from emp e join dept d on e.dept_id = d.id where e.id <= 3",
		"ana|eng;bia|eng;caio|ops")
	// Inner join drops enzo (no department) and the empty department.
	h.expect("select count(*) from emp e inner join dept d on e.dept_id = d.id", "5")
	// Left join keeps enzo, padded with NULLs.
	h.expect("select e.name, d.name from emp e left join dept d on e.dept_id = d.id where e.id >= 5",
		"enzo|NULL;fabi|eng")
	// ...and from the other side keeps the empty department.
	h.expect("select d.name, e.name from dept d left outer join emp e on e.dept_id = d.id where d.id = 3",
		"empty|NULL")
	// A condition in ON is not the same as one in WHERE for an outer join.
	h.expect("select d.name, e.name from dept d left join emp e on e.dept_id = d.id and e.salary > 90 order by 1",
		"empty|NULL;eng|ana;ops|NULL")
	h.expect("select count(*) from dept cross join emp", "18")
	h.expect("select count(*) from dept, emp where dept.id = emp.dept_id", "5")
	// Self join through aliases.
	h.expect("select e.name, b.name from emp e join emp b on e.boss = b.id where e.id in (2, 4)",
		"bia|ana;duda|caio")
	// Three tables, left-associative.
	h.expect("select e.name, d.name, b.name from emp e join dept d on d.id = e.dept_id left join emp b on b.id = e.boss where e.id < 3",
		"ana|eng|NULL;bia|eng|ana")
	// "*" over a join lists every column of both sides, duplicates and all.
	h.expect("select * from dept d join emp e on e.dept_id = d.id where e.id = 3", "2|ops|3|caio|2|60|1")
	h.expect("select d.* from dept d join emp e on e.dept_id = d.id where e.id = 3", "2|ops")

	h.expectError("select id from emp e join dept d on e.dept_id = d.id", pgerr.AmbiguousColumn)
	h.expectError("select x.id from emp e", pgerr.UndefinedTable)
	h.expectError("select e.nope from emp e", pgerr.UndefinedColumn)
	h.expectError("select * from emp e join dept d on e.name", pgerr.DatatypeMismatch)
	h.expectError("select * from emp e join dept d on count(*) > 1", pgerr.GroupingError)
}

func TestAggregates(t *testing.T) {
	h := company(t)
	// Aggregates skip NULLs; count(*) does not.
	h.expect("select count(*), count(salary), sum(salary), min(salary), max(salary), avg(salary) from emp",
		"6|4|300|60|100|75")
	h.expect("select count(distinct salary), sum(distinct salary), count(distinct dept_id) from emp", "3|240|2")
	h.expect("select min(name), max(name) from emp", "ana|fabi")
	// Over no rows: count is 0, the rest NULL, and there is still one row.
	h.expect("select count(*), sum(salary), max(name) from emp where false", "0|NULL|NULL")
	h.expect("select sum(salary) + 1, count(*) * 2 from emp", "301|12")
	h.expect("select avg(salary::float8) from emp where dept_id = 1", "90")

	h.expect("select dept_id, count(*), sum(salary) from emp group by dept_id order by dept_id",
		"1|3|180;2|2|120;NULL|1|NULL")
	h.expect("select d.name, count(e.id) from dept d left join emp e on e.dept_id = d.id group by d.name order by d.name",
		"empty|0;eng|3;ops|2")
	h.expect("select dept_id, count(*) from emp group by dept_id having count(*) > 1 order by 1", "1|3;2|2")
	h.expect("select dept_id from emp group by dept_id having sum(salary) is null", "NULL")
	// Grouping by an expression, by position and by output name.
	h.expect("select salary / 50, count(*) from emp where salary is not null group by salary / 50 order by 1", "1|3;2|1")
	h.expect("select salary / 50 as bucket, count(*) from emp where salary is not null group by 1 order by 1", "1|3;2|1")
	h.expect("select salary / 50 as bucket, count(*) from emp where salary is not null group by bucket order by bucket desc", "2|1;1|3")
	h.expect("select dept_id, salary, count(*) from emp where dept_id = 2 group by dept_id, salary", "2|60|2")
	// An expression built from grouping keys is fine; so is a qualified key.
	h.expect("select e.dept_id * 10 + 1 from emp e where dept_id is not null group by dept_id order by 1", "11;21")
	// GROUP BY without aggregates is DISTINCT.
	h.expect("select dept_id from emp group by dept_id order by dept_id nulls first", "NULL;1;2")
	// HAVING without GROUP BY treats the whole table as one group.
	h.expect("select count(*) from emp having count(*) > 100", "")

	h.expectError("select name, count(*) from emp", pgerr.GroupingError)
	h.expectError("select dept_id, name from emp group by dept_id", pgerr.GroupingError)
	h.expectError("select * from emp group by dept_id", pgerr.GroupingError)
	h.expectError("select dept_id from emp group by dept_id having name = 'x'", pgerr.GroupingError)
	h.expectError("select count(*) from emp where count(*) > 1", pgerr.GroupingError)
	h.expectError("select sum(count(*)) from emp", pgerr.GroupingError)
	h.expectError("select dept_id from emp group by count(*)", pgerr.GroupingError)
	h.expectError("select sum(name) from emp", pgerr.UndefinedFunction)
	h.expectError("select count(*) from emp group by 3", "42P10")
	h.expectError("select upper(distinct name) from emp", pgerr.SyntaxError)
}

func TestOrderLimitDistinct(t *testing.T) {
	h := company(t)
	h.expect("select name from emp order by name desc limit 2", "fabi;enzo")
	// NULLs sort as larger than everything: last ascending, first descending.
	h.expect("select id from emp order by salary, id", "3;4;2;1;5;6")
	h.expect("select id from emp order by salary desc, id", "5;6;1;2;3;4")
	h.expect("select id from emp order by salary nulls first, id desc", "6;5;4;3;2;1")
	h.expect("select id from emp order by salary desc nulls last, id", "1;2;3;4;5;6")
	// By position, by alias, by an expression not in the select list.
	h.expect("select name, salary from emp where salary is not null order by 2 desc, 1", "ana|100;bia|80;caio|60;duda|60")
	h.expect("select name as n from emp order by n limit 1", "ana")
	h.expect("select name from emp order by -id limit 2", "fabi;enzo")
	h.expect("select name from emp order by length(name) desc, name limit 3", "caio;duda;enzo")
	// The sort is stable, so equal keys keep table order.
	h.expect("select id from emp where salary = 60 order by salary", "3;4")
	h.expect("select id from emp order by id limit 2 offset 3", "4;5")
	h.expect("select id from emp order by id offset 4", "5;6")
	h.expect("select id from emp order by id limit 0", "")
	h.expect("select id from emp order by id limit 100 offset 100", "")
	h.expect("select id from emp order by id limit $1 offset $2", "2;3", int64(2), int64(1))
	h.expect("select id from emp order by id limit null", "1;2;3;4;5;6")

	h.expect("select distinct dept_id from emp order by dept_id", "1;2;NULL")
	h.expect("select distinct salary, dept_id from emp where salary is not null order by 1, 2", "60|2;80|1;100|1")
	h.expect("select distinct salary / 100 from emp where salary is not null order by salary / 100", "0;1")
	h.expect("select count(*) from (select distinct dept_id, salary from emp) s", "5")

	h.expectError("select id from emp limit -1", pgerr.InvalidRowCountInLimit)
	h.expectError("select id from emp offset -1", pgerr.InvalidRowCountInOffset)
	h.expectError("select id from emp limit 'x'", pgerr.InvalidTextRepresentation)
	h.expectError("select id from emp limit id", pgerr.UndefinedColumn)
	h.expectError("select id from emp order by 9", "42P10")
	h.expectError("select distinct name from emp order by salary", "42P10")
}

func TestSubqueries(t *testing.T) {
	h := company(t)
	// In FROM.
	h.expect("select s.dept_id, s.total from (select dept_id, sum(salary) as total from emp group by dept_id) s where s.total > 150", "1|180")
	h.expect("select count(*) from (select 1) a, (select 2) b", "1")
	// Scalar, uncorrelated and correlated.
	h.expect("select name from emp where salary = (select max(salary) from emp)", "ana")
	h.expect("select name, (select name from dept where dept.id = emp.dept_id) from emp where id in (1, 5)", "ana|eng;enzo|NULL")
	h.expect("select name from emp e where salary > (select avg(salary) from emp where dept_id = e.dept_id)", "ana")
	h.expect("select d.name, (select count(*) from emp e where e.dept_id = d.id) from dept d order by 1", "empty|0;eng|3;ops|2")
	// EXISTS and IN.
	h.expect("select name from dept d where exists (select 1 from emp e where e.dept_id = d.id) order by name", "eng;ops")
	h.expect("select name from dept d where not exists (select 1 from emp e where e.dept_id = d.id)", "empty")
	h.expect("select name from emp where dept_id in (select id from dept where name = 'ops') order by name", "caio;duda")
	// NOT IN against a set containing NULL is never true: the classic trap.
	h.expect("select count(*) from dept where id not in (select dept_id from emp)", "0")
	h.expect("select count(*) from dept where id not in (select dept_id from emp where dept_id is not null)", "1")
	// A subquery two levels down can still see the outermost row.
	h.expect("select name from dept d where exists (select 1 from emp e where e.dept_id = d.id and e.salary = (select max(salary) from emp x where x.dept_id = d.id)) order by 1", "eng;ops")
	// Used by a writing statement, which already holds the lock.
	h.mustRun("update emp set salary = (select max(salary) from emp) where salary is null")
	h.expect("select count(*) from emp where salary = 100", "3")
	h.mustRun("delete from emp where dept_id in (select id from dept where name = 'ops')")
	h.expect("select count(*) from emp", "4")

	h.expectError("select (select id, name from dept)", pgerr.SyntaxError)
	h.expectError("select (select id from dept)", pgerr.CardinalityViolation)
	h.expectError("select 1 where 1 in (select id, name from dept)", pgerr.SyntaxError)
	h.expectError("select * from (select nope from dept) s", pgerr.UndefinedColumn)
}

func TestCaseInBetweenLike(t *testing.T) {
	h := company(t)
	h.expect("select name, case when salary >= 100 then 'high' when salary >= 70 then 'mid' when salary is null then '?' else 'low' end from emp where id <= 5",
		"ana|high;bia|mid;caio|low;duda|low;enzo|?")
	h.expect("select case dept_id when 1 then 'eng' when 2 then 'ops' end from emp where id in (1, 3, 5)", "eng;ops;NULL")
	// Branch types are unified: integer and double precision give double.
	h.expect("select case when true then 1 else 2.5 end, case when false then 1 else 2.5 end", "1|2.5")
	h.expect("select case when false then 1 end", "NULL")
	h.expect("select coalesce(salary, 0), coalesce(null, null, 7), nullif(dept_id, 1) from emp where id in (1, 5)", "100|7|NULL;0|7|NULL")
	h.expect("select coalesce(salary, 0.5) from emp where id = 5", "0.5")
	h.expect("select abs(-5), abs(2.5), abs(salary - 100) from emp where id = 3", "5|2.5|40")

	h.expect("select 2 in (1, 2), 3 in (1, 2), 3 not in (1, 2)", "t|f|t")
	// Three-valued logic: no match plus a NULL in the list is unknown.
	h.expect("select 3 in (1, null), 1 in (1, null), 3 not in (1, null), null in (1)", "NULL|t|NULL|NULL")
	h.expect("select 1.0 in (1, 2), 'b' in ('a', 'b')", "t|t")
	h.expect("select name from emp where dept_id in (2, 3) order by name", "caio;duda")
	h.expect("select name from emp where id in ($1, $2)", "ana;caio", int64(1), int64(3))

	h.expect("select 5 between 1 and 10, 5 between 6 and 10, 5 not between 6 and 10, 5 between 10 and 1", "t|f|t|f")
	h.expect("select null between 1 and 2, 5 between null and 3", "NULL|f")
	h.expect("select name from emp where salary between 60 and 80 order by name", "bia;caio;duda")

	h.expect("select 'capivara' like 'capi%', 'capivara' like '%vara', 'capivara' like 'c_p%a', 'capivara' like 'capi'", "t|t|t|f")
	h.expect("select 'capivara' like '%', '' like '%', '' like '_', 'a' like '_'", "t|t|f|t")
	h.expect("select 'CAPI' like 'capi', 'CAPI' ilike 'capi', 'capi' not like 'c%'", "f|t|f")
	// A backslash makes the next character literal.
	h.expect(`select '100%' like '100\%', '1000' like '100\%', 'a_b' like 'a\_b', 'axb' like 'a\_b'`, "t|f|t|f")
	// Patterns that need backtracking over several '%'.
	h.expect("select 'aXbXc' like '%X%c', 'abcabc' like '%abc', 'abcab' like '%abc', 'açaí' like '_ça_'", "t|t|f|t")
	h.expect("select null like 'a', 'a' like null", "NULL|NULL")
	h.expect("select name from emp where name like '%a' order by name", "ana;bia;duda")

	h.expectError("select case when 1 then 2 end", pgerr.DatatypeMismatch)
	h.expectError("select case when true then 1 else 'x' || 'y' end", pgerr.DatatypeMismatch)
	h.expectError("select 1 in (true)", pgerr.DatatypeMismatch)
	h.expectError("select 1 like 'a'", pgerr.UndefinedFunction)
}

func TestDefaultsAndConstraints(t *testing.T) {
	h := newHarness(t, New())
	h.mustRun(`create table t (
		a int,
		b int default 7,
		c text default 'x' || 'y' not null,
		d bigint default 1 + 1,
		primary key (a, b),
		unique (a, c))`)
	h.mustRun("insert into t (a) values (1)")
	h.mustRun("insert into t values (2, default, default, default), (3), (4, 4, 'c', default)")
	h.expect("select * from t order by a", "1|7|xy|2;2|7|xy|2;3|7|xy|2;4|4|c|2")

	// The composite primary key: both columns together must be unique...
	h.mustRun("insert into t values (1, 8, 'other', 0)")
	pe := h.expectError("insert into t values (1, 7, 'again', 0)", pgerr.UniqueViolation)
	if pe.Message != `duplicate key value violates unique constraint "t_pkey"` || pe.Detail != "Key (a, b)=(1, 7) already exists." {
		t.Errorf("primary key violation: %q / %q", pe.Message, pe.Detail)
	}
	// ...and neither may be NULL.
	h.expectError("insert into t values (null, 1, 'n', 0)", pgerr.NotNullViolation)
	// The UNIQUE constraint, by its generated name.
	pe = h.expectError("insert into t values (1, 9, 'xy', 0)", pgerr.UniqueViolation)
	if pe.Message != `duplicate key value violates unique constraint "t_a_c_key"` {
		t.Errorf("unique violation: %q", pe.Message)
	}
	// A NULL in a unique key never conflicts.
	h.mustRun("create table u (x int unique, y int); insert into u values (null, 1), (null, 2), (1, 1)")
	if pe := h.expectError("insert into u values (1, 2)", pgerr.UniqueViolation); pe.Message != `duplicate key value violates unique constraint "u_x_key"` {
		t.Errorf("column unique violation: %q", pe.Message)
	}
	h.mustRun("update t set d = d where a = 1") // unchanged keys are not re-checked against themselves
	h.expectError("update t set b = 7 where a = 1 and b = 8", pgerr.UniqueViolation)

	h.expectError("create table bad (a int primary key, b int, primary key (b))", pgerr.InvalidTableDefinition)
	h.expectError("create table bad (a int, primary key (nope))", pgerr.UndefinedColumn)
	h.expectError("create table bad (a int default 'x')", pgerr.InvalidTextRepresentation)
	h.expectError("create table bad (a int default true)", pgerr.DatatypeMismatch)
	h.expectError("create table bad (a int default a)", pgerr.UndefinedColumn)
	h.expectError("update t set a = default_", pgerr.UndefinedColumn)
	h.expectError("select default", pgerr.SyntaxError)

	h.mustRun("update t set b = default where a = 4")
	h.expect("select b from t where a = 4", "7")
}

func TestIndexes(t *testing.T) {
	h := newHarness(t, New())
	h.mustRun("create table t (a int, b text); insert into t values (1, 'x'), (2, 'y'), (2, 'z'), (null, 'n'), (null, 'n')")

	if _, tag, _ := h.run("create index t_b on t (b)"); tag != "CREATE INDEX" {
		t.Errorf("tag: %s", tag)
	}
	// Existing duplicates stop a unique index from being built.
	h.expectError("create unique index t_a on t (a)", pgerr.UniqueViolation)
	h.mustRun("delete from t where b = 'z'")
	h.mustRun("create unique index t_a on t (a)") // the two NULLs do not count as duplicates
	pe := h.expectError("insert into t values (2, 'again')", pgerr.UniqueViolation)
	if pe.Message != `duplicate key value violates unique constraint "t_a"` {
		t.Errorf("message: %q", pe.Message)
	}
	h.mustRun("create index if not exists t_a on t (b)")
	h.expectError("create index t_a on t (b)", pgerr.DuplicateTable)
	h.expectError("create index t on t (b)", pgerr.DuplicateTable) // tables and indexes share a namespace
	h.expectError("create table t_b (x int)", pgerr.DuplicateTable)
	h.expectError("create index i on missing (a)", pgerr.UndefinedTable)
	h.expectError("create index i on t (nope)", pgerr.UndefinedColumn)

	h.mustRun("drop index t_a")
	h.mustRun("insert into t values (2, 'allowed again')")
	h.expectError("drop index t_a", pgerr.UndefinedObject)
	h.mustRun("drop index if exists t_a")

	// DDL is transactional: rolling back restores the index and its rule.
	h.mustRun("delete from t where b = 'allowed again'; create unique index t_a on t (a)")
	h.mustRun("begin; drop index t_a; insert into t values (2, 'dup'); rollback")
	h.expectError("insert into t values (2, 'dup')", pgerr.UniqueViolation)
	h.mustRun("begin; drop table t; rollback")
	h.expectError("create index t_b on t (b)", pgerr.DuplicateTable)
	// Dropping the table drops its indexes, freeing their names.
	h.mustRun("drop table t; create table t_a (x int)")
}

func TestInsertSelect(t *testing.T) {
	h := company(t)
	h.mustRun("create table archive (id int primary key, name text, pay bigint default -1)")
	if _, tag, _ := h.run("insert into archive select id, name, salary from emp where dept_id = 1"); tag != "INSERT 0 3" {
		t.Errorf("tag: %s", tag)
	}
	h.mustRun("insert into archive (name, id) select name, id + 100 from emp where dept_id = 2")
	h.expect("select * from archive order by id", "1|ana|100;2|bia|80;6|fabi|NULL;103|caio|-1;104|duda|-1")
	// The source is read in full before anything is written.
	h.mustRun("insert into archive select id + 1000, name, pay from archive")
	h.expect("select count(*) from archive", "10")
	// All or nothing.
	h.expectError("insert into archive select id + 2000 - id, name, pay from archive", pgerr.UniqueViolation)
	h.expect("select count(*) from archive", "10")

	h.expectError("insert into archive select id from emp, dept", pgerr.AmbiguousColumn)
	h.expectError("insert into archive select id, name, salary, boss from emp", pgerr.SyntaxError)
	h.expectError("insert into archive (id, name) select id from emp", pgerr.SyntaxError)
	h.expectError("insert into archive select name, id, salary from emp", pgerr.DatatypeMismatch)
}

func TestParametersAcrossQueryLevels(t *testing.T) {
	h := company(t)
	// $1 is typed by its use inside the subquery, $2 by the outer query.
	stmts, _ := h.sess.Parse("select name from emp e where exists (select 1 from dept d where d.id = e.dept_id and d.name = $1) and salary > $2")
	p, err := h.sess.Prepare(stmts[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.ParamOIDs(); len(got) != 2 || got[0] != 25 || got[1] != 23 {
		t.Errorf("parameter types: %v", got)
	}
	h.expect("select name from emp e where exists (select 1 from dept d where d.id = e.dept_id and d.name = $1) and salary > $2",
		"ana;bia", "eng", int64(70))
}
