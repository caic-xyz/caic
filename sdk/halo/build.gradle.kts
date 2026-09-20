plugins {
    alias(libs.plugins.android.library)
    alias(libs.plugins.kotlin.android)
    alias(libs.plugins.detekt)
    alias(libs.plugins.ktlint)
}

android {
    namespace = "com.caic.halo.ble"

    compileSdk {
        version = release(36)
    }

    defaultConfig {
        minSdk = 33
        testInstrumentationRunner = "androidx.test.runner.AndroidJUnitRunner"
    }

    buildTypes {
        debug {
            enableUnitTestCoverage = true
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    @Suppress("DEPRECATION")
    kotlinOptions {
        jvmTarget = "17"
    }

    lint {
        warningsAsErrors = true
        abortOnError = true
    }

    testOptions {
        unitTests.isIncludeAndroidResources = true
    }
}

detekt {
    buildUponDefaultConfig = true
    config.setFrom(files("$rootDir/detekt.yml"))
    parallel = true
}

// Robolectric resolves the Android runtime jar for its tests by downloading it from Maven Central
// while the unit tests run, so a repository hiccup aborts a test class mid-build. Resolve the same
// artifact through Gradle and point Robolectric's offline resolver at it instead: the jar is
// checksum-verified and cached with the rest of the build, and the tests need no network access.
val robolectricSdkJar =
    configurations.create("robolectricSdkJar") {
        isCanBeConsumed = false
        isCanBeResolved = true
    }

val robolectricSdkJarDir = layout.buildDirectory.dir("robolectric-sdk-jars")

val stageRobolectricSdkJar =
    tasks.register<Sync>("stageRobolectricSdkJar") {
        from(robolectricSdkJar)
        into(robolectricSdkJarDir)
    }

tasks.withType<Test>().configureEach {
    dependsOn(stageRobolectricSdkJar)
    systemProperty("robolectric.dependency.dir", robolectricSdkJarDir.get().asFile.absolutePath)
}

dependencies {
    // Coroutines — Flow-based API for async BLE operations (exposed to consumers).
    api(libs.kotlinx.coroutines.core)

    // WebSocket client for the development-only Halo emulator bridge.
    implementation(libs.kotlinx.serialization.json)
    implementation(libs.okhttp)

    // Unit tests (JVM with Robolectric shadows)
    testImplementation(libs.junit)
    testImplementation(libs.kotlinx.coroutines.test)
    testImplementation(libs.okhttp.mockwebserver)
    testImplementation(libs.robolectric)
    add(robolectricSdkJar.name, libs.robolectric.android.all)
}

// ktlint formatting and checks for the hand-written Kotlin in this module.
ktlint {
    version.set("1.8.0")
}
