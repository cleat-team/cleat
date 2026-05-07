buildscript {
    repositories {
        mavenCentral()
    }
    dependencies {
        classpath("org.teavm:teavm-gradle-plugin:0.10.2")
    }
}

apply(plugin = "java")
apply(plugin = "org.teavm")

group = "cleatexample.statemachine"
version = "0.1.0"

repositories {
    mavenCentral()
}

dependencies {
    // Cleat durable Java SDK (provides HostCalls, DurableEntry, JsonHelper, etc.)
    implementation(project(":durable-java"))

    // Explicit annotation processor config (processor auto-discovered via META-INF/services)
    annotationProcessor(project(":durable-java"))

    // TeaVM runtime class library for WASM target
    implementation("org.teavm:teavm-classlib:0.10.2")

    // Testing
    testImplementation("org.junit.jupiter:junit-jupiter:5.10.0")
}

java {
    sourceCompatibility = JavaVersion.VERSION_11
    targetCompatibility = JavaVersion.VERSION_11
}

teavm {
    // TeaVM 0.10.2 flat configuration (no nested "wasm {}" block).
    mainClass.set("cleatexample.statemachine.WorkflowEntry")
    fileName.set("workflow.wasm")
    outputDir.set(layout.buildDirectory.dir("wasm"))
    targetType.set("WASM")
    optimizationLevel.set("BALANCED")
    debugInformationGenerated.set(false)
    sourceMapsGenerated.set(false)
}

tasks.test {
    useJUnitPlatform()
}
