import java.sql.Connection;
import java.sql.DriverManager;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.sql.ResultSetMetaData;
import java.sql.SQLException;
import java.sql.Statement;
import java.sql.Types;

/**
 * Smoke test against the PostgreSQL JDBC driver (pgjdbc).
 *
 * Run by scripts/jdbc-smoke.sh as a single-file program:
 *   java -cp postgresql.jar compat/jdbc/Smoke.java jdbc:postgresql://127.0.0.1:PORT/capi
 *
 * Every check prints one line; the process exits non-zero if any fails.
 */
public class Smoke {
    static int failures = 0;

    static void check(String what, Object got, Object want) {
        boolean ok = String.valueOf(got).equals(String.valueOf(want));
        System.out.printf("%s  %-46s %s%n", ok ? "ok  " : "FAIL", what, ok ? got : "got " + got + ", want " + want);
        if (!ok) failures++;
    }

    public static void main(String[] args) throws Exception {
        String url = args.length > 0 ? args[0] : "jdbc:postgresql://127.0.0.1:5432/capi";
        try (Connection conn = DriverManager.getConnection(url, "ana", "")) {
            var meta = conn.getMetaData();
            System.out.println("driver: " + meta.getDriverName() + " " + meta.getDriverVersion());
            check("server version seen by the driver", meta.getDatabaseProductVersion(), "16.0");

            try (Statement st = conn.createStatement()) {
                st.execute("create table users (id int primary key, name text not null, age bigint, score float8, active bool)");
                try (ResultSet rs = st.executeQuery("select version()")) {
                    rs.next();
                    System.out.println("server: " + rs.getString(1));
                }
            }

            // Typed parameters: each setter declares a parameter type OID.
            try (PreparedStatement ins = conn.prepareStatement("insert into users values (?, ?, ?, ?, ?)")) {
                Object[][] rows = {
                    {1, "ana", 30L, 9.5, true},
                    {2, "bia", null, 7.25, false},
                    {3, "caio", 41L, 0.0, true},
                };
                for (Object[] r : rows) {
                    ins.setInt(1, (Integer) r[0]);
                    ins.setString(2, (String) r[1]);
                    if (r[2] == null) ins.setNull(3, Types.BIGINT); else ins.setLong(3, (Long) r[2]);
                    ins.setDouble(4, (Double) r[3]);
                    ins.setBoolean(5, (Boolean) r[4]);
                    check("insert " + r[1], ins.executeUpdate(), 1);
                }
            }

            try (PreparedStatement q = conn.prepareStatement(
                    "select id, name, age, score, active from users where age > ? and active = ?")) {
                // Run it enough times to cross pgjdbc's prepareThreshold (5),
                // after which it switches to a named server-side statement
                // and binary transfer.
                for (int i = 0; i < 7; i++) {
                    q.setLong(1, 35L);
                    q.setBoolean(2, true);
                    try (ResultSet rs = q.executeQuery()) {
                        if (i == 0 || i == 6) {
                            ResultSetMetaData md = rs.getMetaData();
                            check("column types (run " + (i + 1) + ")",
                                md.getColumnTypeName(1) + "," + md.getColumnTypeName(2) + "," + md.getColumnTypeName(3)
                                    + "," + md.getColumnTypeName(4) + "," + md.getColumnTypeName(5),
                                "int4,text,int8,float8,bool");
                            rs.next();
                            check("row (run " + (i + 1) + ")",
                                rs.getInt(1) + "," + rs.getString(2) + "," + rs.getLong(3) + "," + rs.getDouble(4) + "," + rs.getBoolean(5),
                                "3,caio,41,0.0,true");
                            check("no more rows (run " + (i + 1) + ")", rs.next(), false);
                        }
                    }
                }
            }

            try (Statement st = conn.createStatement();
                 ResultSet rs = st.executeQuery("select age from users where id = 2")) {
                rs.next();
                rs.getLong(1);
                check("NULL is reported by wasNull()", rs.wasNull(), true);
            }

            // Errors surface as SQLException with the SQLSTATE we sent.
            try (Statement st = conn.createStatement()) {
                st.execute("insert into users values (1, 'dup', 1, 1, true)");
                check("duplicate key raises", "no exception", "SQLException");
            } catch (SQLException e) {
                check("duplicate key SQLSTATE", e.getSQLState(), "23505");
            }
            try (Statement st = conn.createStatement()) {
                st.executeQuery("select nope from users");
                check("unknown column raises", "no exception", "SQLException");
            } catch (SQLException e) {
                check("unknown column SQLSTATE", e.getSQLState(), "42703");
            }

            // With autocommit off the driver issues BEGIN on its own.
            conn.setAutoCommit(false);
            try (Statement st = conn.createStatement()) {
                check("delete inside a transaction", st.executeUpdate("delete from users where id > 1"), 2);
            }
            conn.rollback();
            check("rows after rollback", count(conn), 3);

            try (PreparedStatement up = conn.prepareStatement("update users set score = score + ? where id = ?")) {
                for (int id = 1; id <= 3; id++) {
                    up.setDouble(1, 1.0);
                    up.setInt(2, id);
                    up.addBatch();
                }
                check("batch update counts", java.util.Arrays.toString(up.executeBatch()), "[1, 1, 1]");
            }
            conn.commit();
            conn.setAutoCommit(true);

            try (Statement st = conn.createStatement();
                 ResultSet rs = st.executeQuery("select score from users where id = 1")) {
                rs.next();
                check("score after committed batch", rs.getDouble(1), 10.5);
            }

            // A real cursor: with autocommit off and a fetch size, the driver
            // asks for the rows fifty at a time and the server keeps the
            // query suspended in between. Half-way through, another
            // connection deletes every row and vacuums; the cursor must go
            // on returning what existed when it was opened.
            try (Statement st = conn.createStatement()) {
                st.executeUpdate("create table seq (n int primary key)");
                st.executeUpdate("insert into seq values (0), (1), (2), (3), (4)");
                for (int size = 5; size < 640; size *= 2) {
                    st.executeUpdate("insert into seq select n + " + size + " from seq");
                }
            }
            conn.setAutoCommit(false);
            try (Statement st = conn.createStatement()) {
                st.setFetchSize(50);
                try (ResultSet rs = st.executeQuery("select n from seq")) {
                    long sum = 0;
                    int rows = 0;
                    while (rs.next()) {
                        sum += rs.getInt(1);
                        if (++rows == 120) {
                            try (Connection other = DriverManager.getConnection(url, "ana", "");
                                 Statement del = other.createStatement()) {
                                check("rows deleted under the open cursor", del.executeUpdate("delete from seq"), 640);
                                del.execute("vacuum seq");
                            }
                        }
                    }
                    check("rows read through the cursor", rows, 640);
                    check("their sum", sum, 640L * 639 / 2);
                }
                try (ResultSet rs = st.executeQuery("select count(*) from seq")) {
                    rs.next();
                    check("rows a new statement sees", rs.getInt(1), 0);
                }
            }
            conn.commit();
            conn.setAutoCommit(true);

            // Set operations and a full join, as seen by the driver.
            try (Statement st = conn.createStatement();
                 ResultSet rs = st.executeQuery(
                     "select a.x, b.x from (select 1 as x union select 2) a "
                     + "full join (select 2 as x union all select 3) b on a.x = b.x order by a.x, b.x")) {
                StringBuilder got = new StringBuilder();
                while (rs.next()) {
                    got.append(rs.getObject(1)).append('|').append(rs.getObject(2)).append(' ');
                }
                check("full join of two unions", got.toString().trim(), "1|null 2|2 null|3");
            }
        }
        if (failures > 0) {
            System.out.println(failures + " check(s) failed");
            System.exit(1);
        }
        System.out.println("all checks passed");
    }

    static int count(Connection conn) throws SQLException {
        try (Statement st = conn.createStatement(); ResultSet rs = st.executeQuery("select id from users")) {
            int n = 0;
            while (rs.next()) n++;
            return n;
        }
    }
}
