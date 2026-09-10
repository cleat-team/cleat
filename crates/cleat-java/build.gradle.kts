plugins {
    java
}

group = "com.cleat"
version = "0.1.0"

repositories {
    mavenCentral()
}

dependencies {
    implementation("org.teavm:teavm-classlib:0.10.2")
    implementation("org.teavm:teavm-jso-apis:0.10.2")
    testImplementation("org.junit.jupiter:junit-jupiter:5.10.0")
    testRuntimeOnly("org.junit.platform:junit-platform-launcher")
}

java {
    sourceCompatibility = JavaVersion.VERSION_11
    targetCompatibility = JavaVersion.VERSION_11
}

tasks.test {
    // The shared conformance table lives at the repo root because it belongs to
    // no single SDK, and QuorumConformanceTest reads it at RUNTIME by relative
    // path. Gradle cannot infer that, so without this declaration the task is
    // "up to date" whenever only the table has changed -- and the test that
    // exists to validate the table is the one that does not re-run.
    //
    // Found by falsifying: corrupting an `awaited_sets` entry produced BUILD
    // SUCCESSFUL in 578ms. The duration was the tell; the tests had not run at
    // all. With --rerun-tasks the same corruption fails correctly.
    // NOT rootProject.file(): this is a standalone build, so rootDir IS
    // crates/cleat-java. Measured with `gradle properties` rather than assumed
    // -- the first version of this line pointed at a path that does not exist,
    // and a declared input that is missing is not an error, it is simply an
    // input that never changes.
    inputs.file(file("../../tests/conformance/quorum_cases.json"))
        .withPropertyName("quorumConformanceTable")
        .withPathSensitivity(PathSensitivity.RELATIVE)

    useJUnitPlatform()
}
