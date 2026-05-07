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
    wasm {
        mainClass = "cleat.HostCalls"
        targetFileName = "workflow.wasm"
        outputDir = file("build/wasm")
        optimization = org.teavm.gradle.api.OptimizationLevel.BALANCED
    }
}

tasks.test {
    useJUnitPlatform()
}
