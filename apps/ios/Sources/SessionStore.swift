import Foundation
import Security

struct Session: Codable, Equatable {
    let apiBase: URL
    let token: String
}

/// The device token lives only in the Keychain, never in UserDefaults or logs.
enum SessionStore {
    struct KeychainError: Error { let status: OSStatus }

    private static let identity: [String: Any] = [
        kSecClass as String: kSecClassGenericPassword,
        kSecAttrService as String: "net.khaledez.homeaccess",
        kSecAttrAccount as String: "session",
    ]

    static func save(_ s: Session) throws {
        clear()
        var item = identity
        item[kSecValueData as String] = try JSONEncoder().encode(s)
        item[kSecAttrAccessible as String] = kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly
        let status = SecItemAdd(item as CFDictionary, nil)
        if status != errSecSuccess { throw KeychainError(status: status) }
    }

    static func load() -> Session? {
        var query = identity
        query[kSecReturnData as String] = true
        query[kSecMatchLimit as String] = kSecMatchLimitOne
        var result: CFTypeRef?
        guard SecItemCopyMatching(query as CFDictionary, &result) == errSecSuccess,
              let data = result as? Data else { return nil }
        return try? JSONDecoder().decode(Session.self, from: data)
    }

    static func clear() {
        SecItemDelete(identity as CFDictionary)
    }
}
