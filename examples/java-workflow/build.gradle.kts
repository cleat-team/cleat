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

group = "com.cleat.example"
version = "0.1.0"

repositories {
    mavenCentral()
}

dependencies {
    implementation(project(":cleat-java"))
    implementation("org.teavm:teavm-classlib:0.10.2")
}

java {
    sourceCompatibility = JavaVersion.VERSION_11
    targetCompatibility = JavaVersion.VERSION_11
}

teavm {
    // TeaVM 0.10.2 configuration:
    // - Flat configuration (no nested "wasm {}" block)
    // - Use .set() for Kotlin DSL Property delegates
    mainClass.set("com.cleat.example.WorkflowEntry")
    fileName.set("workflow.wasm")
    outputDir.set(layout.buildDirectory.dir("wasm"))
    targetType.set("WASM")
    optimizationLevel.set("FULL")
    debugInformationGenerated.set(false)
    sourceMapsGenerated.set(false)
}
