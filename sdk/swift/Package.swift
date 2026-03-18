// swift-tools-version: 5.9
// Swift Package Manager manifest for the CaicSDK client library.
import PackageDescription

let package = Package(
    name: "CaicSDK",
    platforms: [
        .macOS(.v13),
        .iOS(.v16),
    ],
    products: [
        .library(name: "CaicSDK", targets: ["CaicSDK"]),
    ],
    targets: [
        .target(name: "CaicSDK", path: "Sources/CaicSDK"),
    ]
)
