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

    // cleat#4113: the second entry point, for the side-effect test. Same reason as above: without it the build declares the entry and
    // the module does not export it.
    private static final Class<?> SIDE_EFFECT_EXPORT_REF =
        HostCallFixture_sideEffectOnce_Export.class;

    public static void main(String[] args) {
        System.out.println("Cleat host-call fixture: " + HostCallFixture.class.getName());
    }
}
