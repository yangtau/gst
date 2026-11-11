import java.io.File

plugins {
    alias(libs.plugins.android.application)
    alias(libs.plugins.kotlin.compose)
    alias(libs.plugins.kotlin.serialization)
}

// The release of gst, whose single source is internal/version.
val release: String = File(rootDir, "../internal/version/version.go").readLines()
    .firstNotNullOf { Regex("""^const Release = "(.*)"$""").find(it)?.groupValues?.get(1) }

android {
    namespace = "gst.app"
    compileSdk = 37

    defaultConfig {
        // Another identifier, for a build of your own: -PgstApplicationId=… or
        // gstApplicationId in ~/.gradle/gradle.properties.
        applicationId = (findProperty("gstApplicationId") as String?) ?: "gst.app"
        minSdk = 29
        targetSdk = 36
        versionName = release
        versionCode = release.split(".").map { it.toInt() }.let { (major, minor, patch) -> major * 10000 + minor * 100 + patch }
        // The ABIs that build-core.sh builds gst for.
        ndk.abiFilters += listOf("arm64-v8a", "armeabi-v7a", "x86_64")
    }

    // The Go code, which build-core.sh builds for each ABI.
    sourceSets["main"].jniLibs.directories.add("../build/jniLibs")

    buildTypes {
        release {
            isMinifyEnabled = true
            isShrinkResources = true
            proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt"), "proguard-rules.pro")
            // Signed with the debug key unless a release key is configured; see README.md.
            signingConfig = signingConfigs.getByName("debug")
        }
    }

    buildFeatures {
        compose = true
        buildConfig = true
    }

    testOptions {
        unitTests {
            isIncludeAndroidResources = true
            all {
                // Robolectric's Android 16 reaches into the JDK's file descriptors.
                it.jvmArgs("--add-exports=java.base/jdk.internal.access=ALL-UNNAMED", "--add-opens=java.base/java.io=ALL-UNNAMED")
                it.systemProperty("robolectric.pixelCopyRenderMode", "hardware")
                // gst's Go code for this machine, which CoreTest loads: build-core.sh host.
                it.systemProperty("java.library.path", rootProject.layout.buildDirectory.dir("host").get().asFile.path)
                // The tests of the screens keep a picture of each there; see README.md.
                it.systemProperty("roborazzi.test.record", "true")
                it.systemProperty("roborazzi.output.dir", layout.buildDirectory.dir("screenshots").get().asFile.path)
            }
        }
    }

    packaging {
        jniLibs.useLegacyPackaging = false
    }
}

kotlin {
    jvmToolchain(21)
}

dependencies {
    implementation(platform(libs.compose.bom))
    implementation(libs.compose.ui)
    implementation(libs.compose.ui.tooling.preview)
    implementation(libs.compose.material3)
    implementation(libs.compose.icons)
    implementation(libs.activity.compose)
    implementation(libs.lifecycle.runtime.compose)
    implementation(libs.core.ktx)
    implementation(libs.serialization.json)
    implementation(libs.coroutines.android)
    debugImplementation(libs.compose.ui.tooling)
    debugImplementation(libs.compose.ui.test.manifest)

    testImplementation(platform(libs.compose.bom))
    testImplementation(libs.junit)
    testImplementation(libs.robolectric)
    testImplementation(libs.roborazzi)
    testImplementation(libs.roborazzi.compose)
    testImplementation(libs.compose.ui.test.junit4)
    testImplementation(libs.androidx.test.core)
    testImplementation(libs.coroutines.test)
}
