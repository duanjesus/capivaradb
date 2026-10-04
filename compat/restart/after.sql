-- Second server process, same file: everything is still there.
select * from capivaras;

-- So are the rules: the primary key, the unique name, the defaults.
insert into capivaras values (1, 'Outra', 1, true);
insert into capivaras (id, name) values (5, 'Filó');
insert into capivaras (id, name) values (5, 'Nova');
select name, weight, wild from capivaras where id = 5;

-- The offline check above compared the index with the rows, entry by entry.
-- (Queries do not use indexes yet: choosing one is the planner's job.)
select name from capivaras where weight > 46 order by weight;
