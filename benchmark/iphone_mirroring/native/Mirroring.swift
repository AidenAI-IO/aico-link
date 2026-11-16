import AppKit
import ApplicationServices
import ScreenCaptureKit
import Carbon

// One JSON request on stdin, one JSON response on stdout. No permission prompts.
// Keep the executable at a stable path so macOS can retain its permissions.
struct BridgeError: Error, CustomStringConvertible {
    let description: String
    init(_ message: String) { description = message }
}

struct MirrorWindow {
    let app: NSRunningApplication
    let id: CGWindowID
    let frame: CGRect
    let content: CGRect
}

func number(_ value: Any?, _ fallback: Double = 0) -> Double {
    (value as? NSNumber)?.doubleValue ?? fallback
}

func mirrorWindow(_ request: [String: Any]) throws -> MirrorWindow {
    guard let app = NSRunningApplication.runningApplications(withBundleIdentifier: "com.apple.ScreenContinuity").first else {
        throw BridgeError("Open iPhone Mirroring and connect the iPhone first")
    }
    let windows = CGWindowListCopyWindowInfo([.optionOnScreenOnly, .excludeDesktopElements], kCGNullWindowID) as? [[String: Any]] ?? []
    let candidates = windows.filter {
        ($0[kCGWindowOwnerPID as String] as? Int32) == app.processIdentifier &&
        ($0[kCGWindowLayer as String] as? Int) == 0 &&
        // Exclude the app's Settings, Help, and other auxiliary windows.
        (($0[kCGWindowName as String] as? String) == app.localizedName ||
         ($0[kCGWindowName as String] as? String) == "iPhone Mirroring")
    }
    let insets = request["insets"] as? [Double] ?? [8, 38, 8, 8] // left, top, right, bottom, in points
    guard insets.count == 4, insets.allSatisfy({ $0.isFinite && $0 >= 0 }) else {
        throw BridgeError("insets must contain four non-negative numbers")
    }
    for candidate in candidates {
        guard let bounds = candidate[kCGWindowBounds as String] as? [String: Any],
              let frame = CGRect(dictionaryRepresentation: bounds as CFDictionary),
              frame.width >= 200, frame.height >= 300,
              let id = candidate[kCGWindowNumber as String] as? UInt32 else { continue }
        let content = CGRect(x: frame.minX + insets[0], y: frame.minY + insets[1],
                             width: frame.width - insets[0] - insets[2],
                             height: frame.height - insets[1] - insets[3])
        guard content.width > 0, content.height > 0 else { throw BridgeError("insets exceed the mirror window") }
        return MirrorWindow(app: app, id: id, frame: frame, content: content)
    }
    throw BridgeError("No visible iPhone Mirroring window; restore it and connect the iPhone")
}

func matchesMirrorWindow(_ element: AXUIElement, _ window: MirrorWindow) -> Bool {
    var title: CFTypeRef?, position: CFTypeRef?, size: CFTypeRef?
    guard AXUIElementCopyAttributeValue(element, kAXTitleAttribute as CFString, &title) == .success,
          let name = title as? String,
          name == window.app.localizedName || name == "iPhone Mirroring",
          AXUIElementCopyAttributeValue(element, kAXPositionAttribute as CFString, &position) == .success,
          AXUIElementCopyAttributeValue(element, kAXSizeAttribute as CFString, &size) == .success,
          let position, let size,
          CFGetTypeID(position) == AXValueGetTypeID(), CFGetTypeID(size) == AXValueGetTypeID() else { return false }
    var point = CGPoint.zero
    var dimensions = CGSize.zero
    guard AXValueGetValue(position as! AXValue, .cgPoint, &point),
          AXValueGetValue(size as! AXValue, .cgSize, &dimensions) else { return false }
    // Public AX APIs expose window geometry, not the CGWindowID. Match the
    // selected CG window's title and bounds, and reject ambiguous matches.
    return abs(point.x - window.frame.minX) < 1 && abs(point.y - window.frame.minY) < 1 &&
           abs(dimensions.width - window.frame.width) < 1 && abs(dimensions.height - window.frame.height) < 1
}

func focusedMirrorWindow(_ window: MirrorWindow) -> AXUIElement? {
    guard window.app.isActive else { return nil }
    var focused: CFTypeRef?
    let app = AXUIElementCreateApplication(window.app.processIdentifier)
    guard AXUIElementCopyAttributeValue(app, kAXFocusedWindowAttribute as CFString, &focused) == .success,
          let focused, CFGetTypeID(focused) == AXUIElementGetTypeID() else { return nil }
    let element = focused as! AXUIElement
    return matchesMirrorWindow(element, window) ? element : nil
}

func hasInputFocus(_ window: MirrorWindow) -> Bool {
    AXIsProcessTrusted() && CGPreflightPostEventAccess() && focusedMirrorWindow(window) != nil
}

func focus(_ window: MirrorWindow) throws {
    guard AXIsProcessTrusted() && CGPreflightPostEventAccess() else {
        throw BridgeError("Accessibility permission is required for the terminal/bridge executable")
    }
    let target = AXUIElementCreateApplication(window.app.processIdentifier)
    var value: CFTypeRef?
    guard AXUIElementCopyAttributeValue(target, kAXWindowsAttribute as CFString, &value) == .success,
          let windows = value as? [AXUIElement] else {
        throw BridgeError("Cannot identify the iPhone Mirroring window; input was not sent")
    }
    let matches = windows.filter { matchesMirrorWindow($0, window) }
    guard matches.count == 1, let selected = matches.first,
          AXUIElementPerformAction(selected, kAXRaiseAction as CFString) == .success else {
        throw BridgeError("Cannot raise the selected iPhone Mirroring window; input was not sent")
    }
    _ = AXUIElementSetAttributeValue(selected, kAXMainAttribute as CFString, kCFBooleanTrue)
    _ = AXUIElementSetAttributeValue(target, kAXFrontmostAttribute as CFString, kCFBooleanTrue)
    window.app.activate(options: [])
    for _ in 0..<20 {
        if let focused = focusedMirrorWindow(window), CFEqual(focused, selected) { return }
        RunLoop.current.run(until: Date(timeIntervalSinceNow: 0.025))
    }
    throw BridgeError("Cannot focus iPhone Mirroring; input was not sent")
}

func point(_ x: Double, _ y: Double, _ window: MirrorWindow) throws -> CGPoint {
    guard x.isFinite, y.isFinite, (0...1000).contains(x), (0...1000).contains(y) else {
        throw BridgeError("coordinates must be in [0, 1000]")
    }
    return CGPoint(x: window.content.minX + min(window.content.width - 1, x * window.content.width / 1000),
                   y: window.content.minY + min(window.content.height - 1, y * window.content.height / 1000))
}

func mouse(_ type: CGEventType, _ point: CGPoint, clicks: Int = 1) {
    let event = CGEvent(mouseEventSource: nil, mouseType: type, mouseCursorPosition: point, mouseButton: .left)
    event?.setIntegerValueField(.mouseEventClickState, value: Int64(clicks))
    event?.post(tap: .cghidEventTap)
}

let keycodes: [String: CGKeyCode] = [
    "a": 0, "s": 1, "d": 2, "f": 3, "h": 4, "g": 5, "z": 6, "x": 7, "c": 8, "v": 9,
    "b": 11, "q": 12, "w": 13, "e": 14, "r": 15, "y": 16, "t": 17,
    "1": 18, "2": 19, "3": 20, "4": 21, "6": 22, "5": 23, "=": 24, "9": 25,
    "7": 26, "-": 27, "8": 28, "0": 29, "]": 30, "o": 31, "u": 32, "[": 33,
    "i": 34, "p": 35, "enter": 36, "return": 36, "l": 37, "j": 38, "'": 39, "k": 40,
    ";": 41, "\\": 42, ",": 43, "/": 44, "n": 45, "m": 46, ".": 47, "tab": 48,
    "space": 49, "`": 50, "backspace": 51, "escape": 53, "esc": 53, "delete": 117,
    "home": 115, "end": 119, "pageup": 116, "pagedown": 121,
    "left": 123, "right": 124, "down": 125, "up": 126,
]

func key(_ code: CGKeyCode, flags: CGEventFlags = []) {
    // Mirroring forwards modifier transitions to iOS separately. Flags on the
    // character alone can otherwise turn Cmd+A into a literal "a" on the phone.
    let modifiers: [(CGEventFlags, CGKeyCode)] = [(.maskCommand, 55), (.maskShift, 56), (.maskAlternate, 58), (.maskControl, 59)]
    var held: CGEventFlags = []
    for (flag, modifier) in modifiers where flags.contains(flag) {
        held.insert(flag)
        let event = CGEvent(keyboardEventSource: nil, virtualKey: modifier, keyDown: true)
        event?.type = .flagsChanged
        event?.flags = held
        event?.post(tap: .cghidEventTap)
        Thread.sleep(forTimeInterval: 0.015)
    }
    for down in [true, false] {
        let event = CGEvent(keyboardEventSource: nil, virtualKey: code, keyDown: down)
        event?.flags = flags
        event?.post(tap: .cghidEventTap)
        Thread.sleep(forTimeInterval: 0.015)
    }
    for (flag, modifier) in modifiers.reversed() where flags.contains(flag) {
        held.remove(flag)
        let event = CGEvent(keyboardEventSource: nil, virtualKey: modifier, keyDown: false)
        event?.type = .flagsChanged
        event?.flags = held
        event?.post(tap: .cghidEventTap)
        Thread.sleep(forTimeInterval: 0.015)
    }
}

func press(_ keys: [String]) throws {
    var flags: CGEventFlags = []
    var code: CGKeyCode?
    for name in keys.map({ $0.lowercased() }) {
        switch name {
        case "meta", "cmd", "command", "super": flags.insert(.maskCommand)
        case "ctrl", "control": flags.insert(.maskControl)
        case "alt", "option": flags.insert(.maskAlternate)
        case "shift": flags.insert(.maskShift)
        default:
            guard code == nil, let value = keycodes[name] else { throw BridgeError("unsupported key combination") }
            code = value
        }
    }
    guard let code else { throw BridgeError("a non-modifier key is required") }
    key(code, flags: flags)
}

func scroll(_ dx: Double, _ dy: Double, at location: CGPoint, duration: Double) {
    mouse(.mouseMoved, location)
    let steps = max(2, Int(duration * 60))
    var sentX = 0, sentY = 0
    for index in 1...steps {
        let targetX = Int((dx * Double(index) / Double(steps)).rounded())
        let targetY = Int((dy * Double(index) / Double(steps)).rounded())
        let event = CGEvent(scrollWheelEvent2Source: nil, units: .pixel, wheelCount: 2,
                            wheel1: Int32(targetY - sentY), wheel2: Int32(targetX - sentX), wheel3: 0)
        event?.location = location
        event?.setIntegerValueField(.scrollWheelEventIsContinuous, value: 1)
        event?.setIntegerValueField(.scrollWheelEventScrollPhase, value: index == 1 ? 1 : 2)
        event?.post(tap: .cghidEventTap)
        sentX = targetX; sentY = targetY
        Thread.sleep(forTimeInterval: duration / Double(steps))
    }
    let end = CGEvent(scrollWheelEvent2Source: nil, units: .pixel, wheelCount: 2, wheel1: 0, wheel2: 0, wheel3: 0)
    end?.location = location
    end?.setIntegerValueField(.scrollWheelEventIsContinuous, value: 1)
    end?.setIntegerValueField(.scrollWheelEventScrollPhase, value: 4)
    end?.post(tap: .cghidEventTap)
}

func typeText(_ text: String, window: MirrorWindow) throws {
    guard !text.isEmpty, text.utf16.count <= 4096 else { throw BridgeError("text must contain 1–4096 UTF-16 units") }
    let shifted: [Character: String] = ["!": "1", "@": "2", "#": "3", "$": "4", "%": "5", "^": "6",
        "&": "7", "*": "8", "(": "9", ")": "0", "_": "-", "+": "=", "{": "[", "}": "]",
        "|": "\\", ":": ";", "\"": "'", "<": ",", ">": ".", "?": "/", "~": "`"]
    // Mirroring reliably forwards physical ASCII keys. For non-ASCII input,
    // Unicode events avoid Universal Clipboard synchronization and preserve
    // the user's clipboard; their delivery still needs device verification.
    for character in text {
        guard hasInputFocus(window) else {
            throw BridgeError("Input permission or mirror window focus was lost; inspect the field before retrying")
        }
        let name = String(character)
        if let base = shifted[character], let code = keycodes[base] {
            key(code, flags: .maskShift)
            continue
        }
        let special = [" ": "space", "\n": "enter", "\t": "tab"][name]
        if let code = keycodes[special ?? name.lowercased()] {
            key(code, flags: name != name.lowercased() ? .maskShift : [])
            continue
        }
        let units = Array(String(character).utf16)
        for down in [true, false] {
            let event = CGEvent(keyboardEventSource: nil, virtualKey: 0, keyDown: down)
            event?.flags = []
            event?.keyboardSetUnicodeString(stringLength: units.count, unicodeString: units)
            event?.post(tap: .cghidEventTap)
        }
        Thread.sleep(forTimeInterval: 0.015)
    }
}

func inputSourceID(_ source: TISInputSource) -> String {
    guard let value = TISGetInputSourceProperty(source, kTISPropertyInputSourceID) else { return "" }
    return Unmanaged<CFString>.fromOpaque(value).takeUnretainedValue() as String
}

// Insert committed text through the Mac text event path. Temporarily select an
// ASCII layout so an active Pinyin/Japanese IME cannot reinterpret the events.
// Physical keypress/text commands keep using the user's selected input source.
func insertText(_ text: String, replace: Bool, window: MirrorWindow) throws {
    guard !text.isEmpty, text.utf16.count <= 4096,
          !text.unicodeScalars.contains(where: { $0.properties.generalCategory == .control && $0 != "\n" && $0 != "\t" }) else {
        throw BridgeError("text must contain 1–4096 UTF-16 units without control characters other than newline/tab")
    }
    let original = TISCopyCurrentKeyboardInputSource().takeRetainedValue()
    let ascii = TISCopyCurrentASCIICapableKeyboardLayoutInputSource().takeRetainedValue()
    let changed = inputSourceID(original) != inputSourceID(ascii)
    if changed && TISSelectInputSource(ascii) != noErr {
        throw BridgeError("Cannot select the macOS text input source; no text was sent")
    }
    let insertion = Result<Void, Error> {
        if changed { RunLoop.current.run(until: Date(timeIntervalSinceNow: 0.1)) }
        guard inputSourceID(TISCopyCurrentKeyboardInputSource().takeRetainedValue()) == inputSourceID(ascii) else {
            throw BridgeError("macOS input source did not change; no text was sent")
        }
        if replace {
            guard hasInputFocus(window) else {
                throw BridgeError("Input permission or mirror focus was lost; no replacement was sent")
            }
            try press(["meta", "a"])
            RunLoop.current.run(until: Date(timeIntervalSinceNow: 0.1))
        }
        for character in text {
            guard hasInputFocus(window) else {
                throw BridgeError("Input permission or mirror focus was lost; text may be partially inserted. Inspect the field before retrying")
            }
            let units = Array(String(character).utf16)
            for down in [true, false] {
                guard let event = CGEvent(keyboardEventSource: nil, virtualKey: 0, keyDown: down) else {
                    throw BridgeError("Cannot create text event; inspect the field before retrying")
                }
                event.flags = []
                event.keyboardSetUnicodeString(stringLength: units.count, unicodeString: units)
                event.post(tap: .cghidEventTap)
            }
            RunLoop.current.run(until: Date(timeIntervalSinceNow: 0.003))
        }
        // Let the application consume the last events before restoring IME.
        RunLoop.current.run(until: Date(timeIntervalSinceNow: 0.1))
    }
    // Restore on both success and failure. Preserve the insertion error if
    // restoration also fails, so focus loss cannot hide an input-source change.
    if changed {
        let outcome: String
        switch insertion {
        case .success: outcome = "Text was sent"
        case .failure(let error): outcome = String(describing: error)
        }
        guard TISSelectInputSource(original) == noErr else {
            throw BridgeError("\(outcome); the original macOS input source could not be restored. Inspect the field before retrying")
        }
        RunLoop.current.run(until: Date(timeIntervalSinceNow: 0.1))
        guard inputSourceID(TISCopyCurrentKeyboardInputSource().takeRetainedValue()) == inputSourceID(original) else {
            throw BridgeError("\(outcome); the macOS input source did not restore. Inspect the field before retrying")
        }
    }
    try insertion.get()
}

@available(macOS 14.0, *)
func capture(_ window: MirrorWindow, quality: Double) async throws -> [String: Any] {
    guard CGPreflightScreenCaptureAccess() else {
        throw BridgeError("Screen Recording permission is required for the terminal/bridge executable")
    }
    let content = try await SCShareableContent.excludingDesktopWindows(true, onScreenWindowsOnly: true)
    guard let target = content.windows.first(where: { $0.windowID == window.id }) else {
        throw BridgeError("The mirror window disappeared before capture")
    }
    let filter = SCContentFilter(desktopIndependentWindow: target)
    let config = SCStreamConfiguration()
    let scale = CGFloat(filter.pointPixelScale)
    config.width = Int(window.frame.width * scale)
    config.height = Int(window.frame.height * scale)
    config.showsCursor = false
    config.ignoreShadowsSingleWindow = true
    let image = try await SCScreenshotManager.captureImage(contentFilter: filter, configuration: config)
    let sx = CGFloat(image.width) / window.frame.width
    let sy = CGFloat(image.height) / window.frame.height
    let crop = CGRect(x: (window.content.minX - window.frame.minX) * sx,
                      y: (window.content.minY - window.frame.minY) * sy,
                      width: window.content.width * sx, height: window.content.height * sy).integral
    guard let cropped = image.cropping(to: crop),
          let jpeg = NSBitmapImageRep(cgImage: cropped).representation(using: .jpeg, properties: [.compressionFactor: quality]) else {
        throw BridgeError("Cannot encode mirror screenshot")
    }
    return ["image": jpeg.base64EncodedString(), "width": cropped.width, "height": cropped.height]
}

@available(macOS 14.0, *)
@MainActor
func execute(_ request: [String: Any]) async throws -> [String: Any] {
    let command = request["command"] as? String ?? "status"
    if command == "request_input_access" {
        let options = [kAXTrustedCheckOptionPrompt.takeUnretainedValue() as String: true] as CFDictionary
        _ = AXIsProcessTrustedWithOptions(options)
        _ = CGRequestPostEventAccess()
        return ["accessibility": AXIsProcessTrusted() && CGPreflightPostEventAccess()]
    }
    if command == "request_screen_access" {
        return ["screen_recording": CGRequestScreenCaptureAccess()]
    }
    if command == "status" {
        var status: [String: Any] = ["screen_recording": CGPreflightScreenCaptureAccess(),
            "accessibility": AXIsProcessTrusted() && CGPreflightPostEventAccess(), "window_visible": false,
            "input_source": inputSourceID(TISCopyCurrentKeyboardInputSource().takeRetainedValue())]
        do {
            let window = try mirrorWindow(request)
            status.merge(["window_id": window.id, "width": window.content.width, "height": window.content.height,
                "window_visible": true,
                "bounds": [window.content.minX, window.content.minY, window.content.width, window.content.height]]) { _, new in new }
        } catch { status["window_error"] = String(describing: error) }
        return status
    }
    let window = try mirrorWindow(request)
    if command == "capture" { return try await capture(window, quality: number(request["quality"], 0.85)) }
    return try input(request, command: command, window: window)
}

func input(_ request: [String: Any], command: String, window: MirrorWindow) throws -> [String: Any] {
    try focus(window)
    switch command {
    case "click":
        let location = try point(number(request["x"]), number(request["y"]), window)
        let count = max(1, min(2, Int(number(request["clicks"], 1))))
        mouse(.mouseMoved, location)
        for index in 1...count {
            mouse(.leftMouseDown, location, clicks: index)
            Thread.sleep(forTimeInterval: max(0.03, min(5, number(request["hold_sec"], 0.03))))
            mouse(.leftMouseUp, location, clicks: index)
            if index < count { Thread.sleep(forTimeInterval: 0.08) }
        }
    case "move": mouse(.mouseMoved, try point(number(request["x"]), number(request["y"]), window))
    case "scroll":
        let location = try point(number(request["x"], 500), number(request["y"], 500), window)
        scroll(max(-2000, min(2000, number(request["dx"]))), max(-2000, min(2000, number(request["dy"]))),
               at: location, duration: max(0.1, min(2, number(request["duration"], 0.4))))
    case "keypress": try press(request["keys"] as? [String] ?? [])
    case "text": try typeText(request["text"] as? String ?? "", window: window)
    case "insert_text": try insertText(request["text"] as? String ?? "", replace: request["replace"] as? Bool ?? false, window: window)
    default: throw BridgeError("unsupported native command: \(command)")
    }
    return [:]
}

@main struct Main {
    static func main() async {
        var response: [String: Any]
        do {
            let data = FileHandle.standardInput.readDataToEndOfFile()
            guard let request = try JSONSerialization.jsonObject(with: data) as? [String: Any] else {
                throw BridgeError("request must be a JSON object")
            }
            if #available(macOS 14.0, *) {
                response = ["ok": true, "data": try await execute(request)]
            } else { throw BridgeError("macOS 14 or newer is required") }
        } catch { response = ["ok": false, "error": String(describing: error)] }
        let result = try! JSONSerialization.data(withJSONObject: response, options: [.sortedKeys])
        FileHandle.standardOutput.write(result)
        FileHandle.standardOutput.write(Data("\n".utf8))
    }
}
