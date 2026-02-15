// Top-level build file where you can add configuration options common to all sub-projects/modules.
plugins {
    alias(libs.plugins.android.application) apply false
    alias(libs.plugins.kotlin.android) apply false
    alias(libs.plugins.detekt) apply false
    alias(libs.plugins.versions)
}

// Reject unstable candidates (alpha, beta, rc) when checking for updates.
fun isStable(version: String): Boolean {
    val upper = version.uppercase()
    val stableKeyword = listOf("RELEASE", "FINAL", "GA").any { upper.contains(it) }
    val regex = "^[0-9,.v-]+(-r)?$".toRegex()
    return stableKeyword || regex.matches(version)
}

tasks.withType<com.github.benmanes.gradle.versions.updates.DependencyUpdatesTask> {
    rejectVersionIf {
        !isStable(candidate.version) && isStable(currentVersion)
    }
}
