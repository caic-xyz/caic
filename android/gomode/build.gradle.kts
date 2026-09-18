// Gradle configuration for the Go Mode Android shell app.
plugins {
    alias(libs.plugins.android.application)
    alias(libs.plugins.kotlin.android)
    alias(libs.plugins.kotlin.serialization)
    alias(libs.plugins.compose.compiler)
    alias(libs.plugins.detekt)
}

android {
    namespace = "com.fghbuild.gomode"
    compileSdk {
        version = release(36)
    }

    defaultConfig {
        applicationId = "com.fghbuild.gomode"
        minSdk = 33
        targetSdk = 36
        versionCode = 1
        versionName = "1.0"

        testInstrumentationRunner = "androidx.test.runner.AndroidJUnitRunner"
    }

    buildTypes {
        debug {
            enableUnitTestCoverage = true
        }
        release {
            isMinifyEnabled = false
            proguardFiles(
                getDefaultProguardFile("proguard-android-optimize.txt"),
                "proguard-rules.pro"
            )
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

    buildFeatures {
        compose = true
    }

    testOptions {
        unitTests.isIncludeAndroidResources = true
    }

    lint {
        warningsAsErrors = true
        abortOnError = true
        xmlReport = true
        // Go Mode hosts LAN/private backends during the WebView spike.
        disable += setOf(
            "GradleDependency",
            "NewerVersionAvailable",
            "AndroidGradlePluginVersion",
            "InsecureBaseConfiguration",
            "OldTargetApi",
        )
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
val robolectricSdkJar = configurations.create("robolectricSdkJar") {
    isCanBeConsumed = false
    isCanBeResolved = true
}

val robolectricSdkJarDir = layout.buildDirectory.dir("robolectric-sdk-jars")

val stageRobolectricSdkJar = tasks.register<Sync>("stageRobolectricSdkJar") {
    from(robolectricSdkJar)
    into(robolectricSdkJarDir)
}

tasks.withType<Test>().configureEach {
    dependsOn(stageRobolectricSdkJar)
    systemProperty("robolectric.dependency.dir", robolectricSdkJarDir.get().asFile.absolutePath)
}

dependencies {
    implementation(project(":halo-sdk"))
    implementation(project(":gomode-sdk"))
    implementation(project(":mcp-sdk"))
    implementation(project(":voicegateway-sdk"))

    implementation(platform(libs.androidx.compose.bom))
    implementation(libs.androidx.compose.material3)
    implementation(libs.androidx.compose.ui)
    implementation(libs.androidx.compose.ui.tooling.preview)
    implementation(libs.androidx.compose.material.icons.extended)
    debugImplementation(libs.androidx.compose.ui.tooling)
    implementation(libs.androidx.activity.compose)
    implementation(libs.androidx.lifecycle.runtime.compose)
    implementation(libs.androidx.datastore.preferences)
    implementation(libs.androidx.core.ktx)
    implementation(libs.androidx.webkit)
    implementation(libs.androidx.credentials)
    implementation(libs.androidx.credentials.play.services.auth)
    implementation(libs.kotlinx.serialization.json)
    implementation(libs.okhttp)
    implementation(libs.stream.webrtc)

    testImplementation(libs.junit)
    testImplementation(libs.kotlinx.coroutines.test)
    testImplementation(libs.okhttp.mockwebserver)
    testImplementation(libs.robolectric)
    add(robolectricSdkJar.name, libs.robolectric.android.all)
    androidTestImplementation(libs.androidx.junit)
    androidTestImplementation(libs.androidx.test.runner)
    androidTestImplementation(libs.androidx.test.rules)
    androidTestImplementation(libs.androidx.test.core)
    androidTestImplementation(libs.androidx.espresso.core)
    androidTestImplementation(platform(libs.androidx.compose.bom))
    androidTestImplementation(libs.androidx.compose.ui.test.junit4)
    androidTestImplementation(libs.androidx.test.uiautomator)
    debugImplementation(libs.androidx.compose.ui.test.manifest)
}
