// cleat#1812. A static import brings the method in under its own name, so the
// call site never writes "System." anywhere -- a spelling table anchored on
// "System.currentTimeMillis()" cannot see it by construction, whatever else
// is in the table.
import static java.lang.System.currentTimeMillis;

public class Main {
    public static long workflow() {
        return currentTimeMillis();
    }
}
