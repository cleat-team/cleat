package com.cleat.harness;

/**
 * TeaVM entry point for the Java host-call fixture.
 *
 * <p>Provides {@code main} as the root of TeaVM's reachability analysis, and
 * holds a static reference to the generated export wrapper so tree-shaking
 * cannot remove the {@code @CleatEntry} method. Without that reference the
 * build succeeds and produces a module with no usable export, which fails at
 * instantiation rather than at build time.
 */
public class HostCallEntry {

    private static final Class<?> EXPORT_REF =
        HostCallFixture_exerciseHostCall_Export.class;

    public static void main(String[] args) {
        System.out.println("Cleat host-call fixture: " + HostCallFixture.class.getName());
    }
}
