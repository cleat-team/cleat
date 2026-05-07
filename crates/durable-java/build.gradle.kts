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

group = "com.cleat"
version = "0.1.0"

repositories {
    mavenCentral()
}

dependencies {
    implementation("org.teavm:teavm-classlib:0.10.2")
    implementation("org.teavm:teavm-jso-apis:0.10.2")
    testImplementation("org.junit.jupiter:junit-jupiter:5.10.0")
}

java {
    sourceCompatibility = JavaVersion.VERSION_11
    targetCompatibility = JavaVersion.VERSION_11
}

teavm {
    // TeaVM 0.10.2 uses a flat configuration (no nested "wasm {}" block)
    // and Gradle Property delegates for all values.
    mainClass.set("cleat.HostCalls")
    fileName.set("workflow.wasm")
    outputDir.set(layout.buildDirectory.dir("wasm"))
    targetType.set("WASM")
    optimizationLevel.set("BALANCED")
}

tasks.test {
    useJUnitPlatform()
}
