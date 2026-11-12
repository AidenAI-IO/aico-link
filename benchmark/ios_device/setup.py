"""Build/install a signed WDA runner or the optional permission test app on macOS."""
from __future__ import annotations

import argparse
import json
import shutil
import subprocess
from pathlib import Path


def probe_project(destination: Path, team: str, bundle: str):
    source = Path(__file__).parent / "permission_probe/App.swift"
    settings = {
        "PRODUCT_NAME": "PermissionProbe", "PRODUCT_BUNDLE_IDENTIFIER": bundle,
        "DEVELOPMENT_TEAM": team, "CODE_SIGN_STYLE": "Automatic", "SDKROOT": "iphoneos",
        "SWIFT_VERSION": "5.0", "IPHONEOS_DEPLOYMENT_TARGET": "16.0", "TARGETED_DEVICE_FAMILY": "1,2",
        "ALWAYS_SEARCH_USER_PATHS": "NO",
        "GENERATE_INFOPLIST_FILE": "YES", "INFOPLIST_KEY_CFBundleDisplayName": "Aico Permission Probe",
        "INFOPLIST_KEY_NSCameraUsageDescription": "Verify camera permission automation. No images are captured.",
        "INFOPLIST_KEY_NSLocationWhenInUseUsageDescription": "Verify location permission automation. No location is read.",
        "INFOPLIST_KEY_UILaunchScreen_Generation": "YES", "CURRENT_PROJECT_VERSION": "1", "MARKETING_VERSION": "1.0",
        "INFOPLIST_KEY_UIApplicationSceneManifest_Generation": "YES",
    }
    config = " ".join(f"{key} = {json.dumps(value)};" for key, value in settings.items())
    project = destination / "PermissionProbe.xcodeproj"
    project.mkdir(parents=True, exist_ok=True)
    (project / "project.pbxproj").write_text('''// !$*UTF8*$!
{ archiveVersion = 1; objectVersion = 56; objects = {
 A1 = {isa = PBXProject; mainGroup = A2; productRefGroup = A3; buildConfigurationList = A4; compatibilityVersion = "Xcode 14.0"; targets = (A5); attributes = {LastUpgradeCheck = 2700;};};
 A2 = {isa = PBXGroup; sourceTree = "<group>"; children = (A3, A6);};
 A3 = {isa = PBXGroup; name = Products; sourceTree = "<group>"; children = (A7);};
 A4 = {isa = XCConfigurationList; buildConfigurations = (A8); defaultConfigurationName = Debug;};
 A5 = {isa = PBXNativeTarget; name = PermissionProbe; productName = PermissionProbe; productType = "com.apple.product-type.application"; productReference = A7; buildConfigurationList = A9; buildPhases = (B1);};
 A6 = {isa = PBXFileReference; lastKnownFileType = sourcecode.swift; path = SOURCE_PATH; sourceTree = "<absolute>";};
 A7 = {isa = PBXFileReference; explicitFileType = wrapper.application; path = PermissionProbe.app; sourceTree = BUILT_PRODUCTS_DIR;};
 A8 = {isa = XCBuildConfiguration; name = Debug; buildSettings = {};};
 A9 = {isa = XCConfigurationList; buildConfigurations = (B2); defaultConfigurationName = Debug;};
 B1 = {isa = PBXSourcesBuildPhase; files = (B3);};
 B2 = {isa = XCBuildConfiguration; name = Debug; buildSettings = {BUILD_SETTINGS};};
 B3 = {isa = PBXBuildFile; fileRef = A6;};
}; rootObject = A1; }
'''.replace("SOURCE_PATH", json.dumps(str(source.resolve()))).replace("BUILD_SETTINGS", config))
    return project


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--team-id", required=True, help="Your Apple Development team")
    parser.add_argument("--udid", required=True, help="Target device identifier")
    parser.add_argument("--output", required=True, type=Path, help="Dedicated local build directory outside tracked source")
    parser.add_argument("--wda-source", type=Path, help="WDA source checkout; copied before customizing its bundle identifier")
    parser.add_argument("--probe-app", action="store_true", help="Build the permission test app instead of WDA")
    parser.add_argument("--bundle-id", help="Custom app/runner bundle identifier")
    parser.add_argument("--install", action="store_true", help="Install the resulting signed app")
    args = parser.parse_args(argv)
    output = args.output.resolve()
    if output.exists():
        parser.error("output must be a new dedicated directory")
    output.mkdir(parents=True)
    if args.probe_app:
        project = probe_project(output, args.team_id, args.bundle_id or "io.aiden.aico.PermissionProbe")
        scheme = "PermissionProbe"
        action = "build"
        app_name = "PermissionProbe.app"
    else:
        if not args.wda_source or not (args.wda_source / "WebDriverAgent.xcodeproj").is_dir():
            parser.error("WDA build requires --wda-source pointing at a WebDriverAgent checkout")
        source = output / "WebDriverAgent"
        shutil.copytree(args.wda_source, source, ignore=shutil.ignore_patterns(".git", "build", "DerivedData"))
        project = source / "WebDriverAgent.xcodeproj"
        pbx = project / "project.pbxproj"
        bundle = args.bundle_id or "io.aiden.aico.WebDriverAgentRunner"
        original = "PRODUCT_BUNDLE_IDENTIFIER = com.facebook.WebDriverAgentRunner;"
        if original not in pbx.read_text():
            parser.error("WDA source does not contain the expected runner bundle setting; use an unmodified checkout")
        pbx.write_text(pbx.read_text().replace(original, f"PRODUCT_BUNDLE_IDENTIFIER = {json.dumps(bundle)};"))
        scheme, action, app_name = "WebDriverAgentRunner", "build-for-testing", "WebDriverAgentRunner-Runner.app"
    subprocess.run(["xcrun", "xcodebuild", "-project", str(project), "-scheme", scheme,
                    "-configuration", "Debug", "-destination", "id=" + args.udid,
                    "-derivedDataPath", str(output / "DerivedData"),
                    "DEVELOPMENT_TEAM=" + args.team_id, "CODE_SIGN_STYLE=Automatic",
                    "-allowProvisioningUpdates", "-allowProvisioningDeviceRegistration", action], check=True)
    app = output / "DerivedData/Build/Products/Debug-iphoneos" / app_name
    if args.install:
        subprocess.run(["xcrun", "devicectl", "device", "install", "app", "--device", args.udid, str(app)], check=True)
    print(app)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
