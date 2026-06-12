// swift-tools-version: 5.9
// Swift Package Manager manifest for the GoModeSDK client library.
import PackageDescription

let package = Package(
    name: "GoModeSDK",
    platforms: [
        .macOS(.v13),
        .iOS(.v16),
    ],
    products: [
        .library(name: "GoModeSDK", targets: ["GoModeSDK"]),
    ],
    targets: [
        .target(name: "GoModeSDK", path: "Sources/GoModeSDK"),
    ]
)
