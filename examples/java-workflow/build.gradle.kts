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
    implementation(project(":durable-java"))
    implementation("org.teavm:teavm-classlib:0.10.2")
}

java {
    sourceCompatibility = JavaVersion.VERSION_11
    targetCompatibility = JavaVersion.VERSION_11
}

teavm {
    mainClass = "com.cleat.example.WorkflowEntry"
    targetFileName = "workflow.wasm"
    targetDirectory = file("build/wasm")
    targetType = "WASM"
    optimizationLevel = "FULL"
    debugInformationGenerated = false
    sourceMapsGenerated = false
}
