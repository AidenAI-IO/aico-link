import UIKit
import UserNotifications
import CoreLocation
import AVFoundation

private extension UNAuthorizationStatus {
    var displayName: String {
        switch self {
        case .notDetermined: return "Not determined"
        case .denied: return "Denied"
        case .authorized: return "Authorized"
        case .provisional: return "Provisional"
        case .ephemeral: return "Ephemeral"
        @unknown default: return "Unknown (\(rawValue))"
        }
    }
}

private extension CLAuthorizationStatus {
    var displayName: String {
        switch self {
        case .notDetermined: return "Not determined"
        case .restricted: return "Restricted"
        case .denied: return "Denied"
        case .authorizedAlways: return "Authorized (always)"
        case .authorizedWhenInUse: return "Authorized (when in use)"
        @unknown default: return "Unknown (\(rawValue))"
        }
    }
}

private extension AVAuthorizationStatus {
    var displayName: String {
        switch self {
        case .notDetermined: return "Not determined"
        case .restricted: return "Restricted"
        case .denied: return "Denied"
        case .authorized: return "Authorized"
        @unknown default: return "Unknown (\(rawValue))"
        }
    }
}

@main
final class AppDelegate: UIResponder, UIApplicationDelegate {
    func application(_ application: UIApplication, configurationForConnecting session: UISceneSession, options: UIScene.ConnectionOptions) -> UISceneConfiguration {
        let configuration = UISceneConfiguration(name: "Default", sessionRole: session.role)
        configuration.delegateClass = SceneDelegate.self
        return configuration
    }
}

final class SceneDelegate: UIResponder, UIWindowSceneDelegate {
    var window: UIWindow?

    func scene(_ scene: UIScene, willConnectTo session: UISceneSession, options: UIScene.ConnectionOptions) {
        guard let scene = scene as? UIWindowScene else { return }
        let window = UIWindow(windowScene: scene)
        window.rootViewController = ProbeController()
        window.makeKeyAndVisible()
        self.window = window
    }
}

final class ProbeController: UIViewController, CLLocationManagerDelegate, UITextFieldDelegate {
    private let location = CLLocationManager()
    private let status = UILabel()

    override func viewDidLoad() {
        super.viewDidLoad()
        view.backgroundColor = .systemBackground
        location.delegate = self
        let title = UILabel()
        title.text = "Aico Permission Probe"
        title.font = .preferredFont(forTextStyle: .title1)
        let explanation = UILabel()
        explanation.text = "Test notification, location and camera permissions. This app does not capture images, read your location or send notifications."
        explanation.numberOfLines = 0
        status.numberOfLines = 0
        status.accessibilityIdentifier = "permission_status"
        let stack = UIStackView(arrangedSubviews: [title, explanation, status])
        stack.axis = .vertical
        stack.spacing = 24
        let field = UITextField()
        field.placeholder = "Text input test"
        field.borderStyle = .roundedRect
        field.accessibilityIdentifier = "input_text"
        field.delegate = self
        field.returnKeyType = .done
        let toolbar = UIToolbar()
        let done = UIBarButtonItem(barButtonSystemItem: .done, target: self, action: #selector(dismissKeyboard))
        done.accessibilityIdentifier = "dismiss_keyboard"
        toolbar.items = [UIBarButtonItem(barButtonSystemItem: .flexibleSpace, target: nil, action: nil), done]
        toolbar.sizeToFit()
        field.inputAccessoryView = toolbar
        stack.addArrangedSubview(field)
        for (label, identifier, action) in [
            ("Request notifications", "request_notification", #selector(requestNotifications)),
            ("Request location", "request_location", #selector(requestLocation)),
            ("Request camera", "request_camera", #selector(requestCamera)),
            ("Refresh status", "refresh_status", #selector(refreshStatus))
        ] {
            let button = UIButton(type: .system)
            button.setTitle(label, for: .normal)
            button.accessibilityIdentifier = identifier
            button.addTarget(self, action: action, for: .touchUpInside)
            stack.addArrangedSubview(button)
        }
        let scroll = UIScrollView()
        scroll.keyboardDismissMode = .interactive
        scroll.translatesAutoresizingMaskIntoConstraints = false
        stack.translatesAutoresizingMaskIntoConstraints = false
        view.addSubview(scroll)
        scroll.addSubview(stack)
        NSLayoutConstraint.activate([
            scroll.topAnchor.constraint(equalTo: view.safeAreaLayoutGuide.topAnchor),
            scroll.leadingAnchor.constraint(equalTo: view.safeAreaLayoutGuide.leadingAnchor),
            scroll.trailingAnchor.constraint(equalTo: view.safeAreaLayoutGuide.trailingAnchor),
            scroll.bottomAnchor.constraint(equalTo: view.keyboardLayoutGuide.topAnchor),
            stack.topAnchor.constraint(equalTo: scroll.contentLayoutGuide.topAnchor, constant: 32),
            stack.leadingAnchor.constraint(equalTo: scroll.contentLayoutGuide.leadingAnchor, constant: 24),
            stack.trailingAnchor.constraint(equalTo: scroll.contentLayoutGuide.trailingAnchor, constant: -24),
            stack.bottomAnchor.constraint(equalTo: scroll.contentLayoutGuide.bottomAnchor, constant: -32),
            stack.widthAnchor.constraint(equalTo: scroll.frameLayoutGuide.widthAnchor, constant: -48)
        ])
        refreshStatus()
    }

    @objc private func dismissKeyboard() { view.endEditing(true) }

    func textFieldShouldReturn(_ textField: UITextField) -> Bool {
        textField.resignFirstResponder()
        return true
    }

    @objc private func requestNotifications() {
        UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound, .badge]) { _, _ in
            DispatchQueue.main.async { self.refreshStatus() }
        }
    }

    @objc private func requestLocation() { location.requestWhenInUseAuthorization() }

    @objc private func requestCamera() {
        AVCaptureDevice.requestAccess(for: .video) { _ in
            DispatchQueue.main.async { self.refreshStatus() }
        }
    }

    func locationManagerDidChangeAuthorization(_ manager: CLLocationManager) { refreshStatus() }

    @objc private func refreshStatus() {
        UNUserNotificationCenter.current().getNotificationSettings { settings in
            DispatchQueue.main.async {
                self.status.text = "Notifications: \(settings.authorizationStatus.displayName)\nLocation: \(self.location.authorizationStatus.displayName)\nCamera: \(AVCaptureDevice.authorizationStatus(for: .video).displayName)"
            }
        }
    }
}
