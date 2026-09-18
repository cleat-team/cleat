// cleat#1812. The old table matched the DECLARATION "Connection con" as a
// literal string, so a JDBC handle named anything else escaped. This is the
// exact same hazard -- a live database connection reaching workflow code --
// spelled with an ordinary variable name. Measured 2026-09-17 against the old
// checker: 0 errors.
import java.sql.Connection;

public class Main {
    public static void workflow(Connection db) throws Exception {
        db.createStatement().execute("SELECT 1");
    }
}
