plugins {
    java
    id("org.teavm") version "0.10.2"
}

group = "com.cleat.example"
version = "0.1.0"

repositories {
    mavenCentral()
}

dependencies {
    implementation(project(":cleat-java"))
    annotationProcessor(project(":cleat-java"))
    implementation("org.teavm:teavm-classlib:0.10.2")
}

java {
    sourceCompatibility = JavaVersion.VERSION_11
    targetCompatibility = JavaVersion.VERSION_11
}

teavm {
    // TeaVM 0.10.2, matching examples/saga-java-port's proven-working
    // Groovy build.gradle: a NESTED "wasm {}" block, not the flat
    // Property.set() style this file assumed before cleat#1890. Kotlin DSL
    // only synthesizes typed accessors for the shape the plugin actually
    // exposes, and at this pinned version that shape is nested per target.
    wasm {
        // Use the generated WorkflowEntry as the analysis root.
        // WorkflowEntry -> CleatEntryIndex -> *_Export classes -> user methods.
        // This reference chain preserves all @CleatEntry exports automatically.
        //
        // .set(), not "=" -- these are Property<T>/DirectoryProperty, not
        // plain vars. saga-java-port's Groovy sibling can use "=" because
        // Groovy's dynamic dispatch accepts it as sugar for .set(); Kotlin
        // DSL does not extend that sugar to a plugin's own Property fields.
        mainClass.set("cleat.WorkflowEntry")
        targetFileName.set("workflow.wasm")
        outputDir.set(file("build/wasm"))
        optimization.set(org.teavm.gradle.api.OptimizationLevel.BALANCED)
    }
}
