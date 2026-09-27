// KNOWN LIMIT -- this file is non-deterministic and the Java checker does not
// see it. That is the point of the fixture; it is not a bug to be fixed here.
//
// (Quoting the patterns in a comment is safe on this side: vet_java.go skips
// comment lines, verified behaviourally rather than by reading -- a comment
// naming System.currentTimeMillis() reports 0 errors while the same text as
// code reports 1. Its Rust sibling does NOT skip comments, which is cleat#1782
// and is why the Rust fixture's comment has to talk around the spellings.)
//
// forbiddenJavaPatterns covers java.io, java.net, java.sql, java.time and
// java.util.concurrent. It does not mention java.nio ANYWHERE:
//
//   $ grep -c 'java\.nio' cmd/cleat/vet_java.go
//   0
//
// java.nio.file is not an exotic corner. It is the API Java has recommended
// over java.io for filesystem work since 1.7, so the escape here is the
// spelling a modern codebase is most likely to use -- the checker covers the
// legacy API and misses its replacement. Measured 2026-09-17:
//
//   cleat vet --lang java testdata/vet-checks/java/known_limit_nio
//   Summary: 1 files, 0 errors, 1 warnings     exit=0
//
// The companion fixture e001_timestamp uses System.currentTimeMillis() and IS
// caught. The pair is asserted together in
// java_build_refuses_nondeterminism_test.go so that "the gate is wired" and
// "the gate is weak" are both stated, and neither can be mistaken for the
// other.
import java.nio.file.Files;
import java.nio.file.Path;

public class Main {
    public static String workflow() throws Exception {
        // Non-deterministic: file contents differ between runs.
        return Files.readString(Path.of("data.txt"));
    }
}
