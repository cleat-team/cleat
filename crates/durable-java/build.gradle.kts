plugins {
    java
    id("org.teavm") version "0.10.2"
}

group = "com.cleat"
version = "0.1.0"

repositories {
    mavenCentral()
}

dependencies {
    // TeaVM runtime
    implementation("org.teavm:teavm-classlib:0.10.2")

    // JSON processing (minimal)
    implementation("org.teavm:teavm-jso-apis:0.10.2")

    // Annotation processing (for javax.annotation.processing at compile time)
    annotationProcessor("org.teavm:teavm-classlib:0.10.2")

    testImplementation("org.junit.jupiter:junit-jupiter:5.10.0")
}

java {
    sourceCompatibility = JavaVersion.VERSION_11
    targetCompatibility = JavaVersion.VERSION_11
}

teavm {
    // TeaVM configuration for WASM target
    mainClass = "cleat.WorkflowEntry"
    targetFileName = "workflow.wasm"
    targetDirectory = file("build/wasm")

    // WASM target with linear memory support for cleat ABI
    targetType = "WASM"

    // Optimize
    optimizationLevel = "FULL"

    // No runtime checks in release
    debugInformationGenerated = false
    sourceMapsGenerated = false

    // Minify (reduces WASM binary size)
    minifying = true
}

tasks.test {
    useJUnitPlatform()
}

// Ensure generated sources from annotation processing are included
sourceSets {
    main {
        java {
            // Include annotation processor generated sources
            srcDir(layout.buildDirectory.dir("generated/source/apt/main"))
        }
    }
}
