pluginManagement {
    repositories {
        google {
            content {
                includeGroupByRegex("com\\.android.*")
                includeGroupByRegex("com\\.google.*")
                includeGroupByRegex("androidx.*")
            }
        }
        mavenCentral()
        gradlePluginPortal()
    }
}
plugins {
    id("org.gradle.toolchains.foojay-resolver-convention") version "1.0.0"
}
dependencyResolutionManagement {
    repositoriesMode.set(RepositoriesMode.FAIL_ON_PROJECT_REPOS)
    repositories {
        google()
        mavenCentral()
    }
}

rootProject.name = "caic"
include(":caic")
include(":gomode")
include(":caic-sdk")
project(":caic-sdk").projectDir = file("../sdk/caic/kotlin")
include(":voicegateway-sdk")
project(":voicegateway-sdk").projectDir = file("../sdk/voicegateway/kotlin")
include(":halo-sdk")
project(":halo-sdk").projectDir = file("../sdk/halo")
