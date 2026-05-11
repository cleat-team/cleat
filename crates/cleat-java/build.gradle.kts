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
    useJUnitPlatform()
}
