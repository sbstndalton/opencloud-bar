// swift-tools-version:5.9
import PackageDescription

let package = Package(
    name: "OpenCloudBar",
    platforms: [.macOS(.v14)],
    targets: [
        .executableTarget(name: "OpenCloudBar", path: "Sources/OpenCloudBar")
    ]
)
