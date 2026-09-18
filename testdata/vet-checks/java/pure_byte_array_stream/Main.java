// cleat#1812. MUST ALLOW. ByteArrayInputStream is pure and in-memory: no
// filesystem, no OS-backed stream, fully replayable from the bytes it was
// constructed with. The old checker refused this file with TWO errors on one
// line -- J008 from the substring "new java.io." and J016 from "InputStream"
// being a substring of "ByteArrayInputStream" -- neither of which is evidence
// of anything non-deterministic. Measured 2026-09-17 against the old checker.
public class Main {
    public static int workflow(byte[] data) {
        java.io.ByteArrayInputStream s = new java.io.ByteArrayInputStream(data);
        return s.read();
    }
}
