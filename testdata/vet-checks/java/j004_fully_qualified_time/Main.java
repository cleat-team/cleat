// cleat#1812. FULLY QUALIFIED, no import at all -- the old substring table
// had no "java.time." pattern that reached Clock (import java.time. was
// removed as an over-broad package ban; see forbiddenJavaPaths's comment for
// why the replacement names only Clock and Instant). This escaped every
// version of the checker before the resolver: measured 2026-09-17,
// `cleat vet --lang java` reported 0 errors against this exact file.
public class Main {
    public static long workflow() {
        return java.time.Clock.systemUTC().millis();
    }
}
