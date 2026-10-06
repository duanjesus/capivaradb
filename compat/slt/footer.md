
## The records that fail

All fifteen are places where the scripts expect SQLite's behaviour and
PostgreSQL itself answers differently, so they are left as they are:

- `1 IN ()` — an empty list is a syntax error in PostgreSQL.
- `x'303132'` — SQLite's blob literal.
- `'hello' IN (SELECT integer_column ...)` — SQLite compares across types;
  PostgreSQL rejects the text as an invalid integer.
- `UPDATE t1 SET x=3, x=4, x=5` — SQLite applies the last assignment;
  PostgreSQL rejects assigning a column twice. The two "wrong result"
  records are queries that depend on that update having happened.
