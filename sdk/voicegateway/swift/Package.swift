// swift-tools-version: 5.9
// Swift Package Manager manifest for the VoiceGatewaySDK client library.
import PackageDescription

let package = Package(
    name: "VoiceGatewaySDK",
    platforms: [
        .macOS(.v13),
        .iOS(.v16),
    ],
    products: [
        .library(name: "VoiceGatewaySDK", targets: ["VoiceGatewaySDK"]),
    ],
    targets: [
        .target(name: "VoiceGatewaySDK", path: "Sources/VoiceGatewaySDK"),
    ]
)
