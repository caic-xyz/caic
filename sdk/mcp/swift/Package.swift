// swift-tools-version: 5.9
// Swift Package Manager manifest for the MCPSDK client library.
import PackageDescription

let package = Package(
    name: "MCPSDK",
    platforms: [
        .macOS(.v13),
        .iOS(.v16),
    ],
    products: [
        .library(name: "MCPSDK", targets: ["MCPSDK"]),
    ],
    targets: [
        .target(name: "MCPSDK", path: "Sources/MCPSDK"),
    ]
)
