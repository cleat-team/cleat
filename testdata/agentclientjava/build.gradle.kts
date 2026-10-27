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
    // Mirrors examples/java-workflow/build.gradle.kts exactly -- same TeaVM
    // version, same nested "wasm {}" shape (cleat#1890: a flat Property.set()
    // style does not match what this plugin version exposes).
    wasm {
        mainClass.set("cleat.WorkflowEntry")
        targetFileName.set("workflow.wasm")
        outputDir.set(file("build/wasm"))
        optimization.set(org.teavm.gradle.api.OptimizationLevel.BALANCED)

        // cleat#2978: this client's first host call writes only a handful
        // of bytes before reaching cleat_child_workflow -- not enough for
        // TeaVM's own GC to have grown linear memory past cleat's fixed
        // scratch-and-output region (Memory.SCRATCH_BASE through
        // Memory.OUTPUT_OFFSET + Memory.OUT_BUF_SIZE, ~10.1 MiB; see
        // Memory.java) by that point. The host cannot grow the guest's
        // memory itself, so its write into OUTPUT_OFFSET then landed past
        // the end of linear memory -- silently, as an empty result rather
        // than an error, because an out-of-bounds write is not the
        // truncation case engine/flush.go's writeOut was written to signal.
        //
        // minHeapSize/maxHeapSize are whole megabytes (TeaVMTask converts
        // by multiplying by 1048576), defaulting to 1/16 -- NOT bytes and
        // NOT WASM pages, both of which were tried and produced a 2-page
        // module that trapped in GC's own clinit, because a minHeapSize
        // above the default 16 MB maxHeapSize is an invalid (min > max)
        // configuration. Setting both keeps them consistent and gives the
        // ABI region headroom over the program's own heap.
        minHeapSize.set(12)
        maxHeapSize.set(32)
    }
}
