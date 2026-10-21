import Foundation

struct Invite: Equatable {
    let apiBase: URL
    let code: String
}

func parseInviteQR(_ raw: String) -> Invite? {
    guard var parts = URLComponents(string: raw) else { return nil }

    let isHTTPS = parts.scheme == "https"
    let hasHost = !(parts.host ?? "").isEmpty
    let isEnrollPath = parts.path.hasSuffix("/guest/enroll")
    let code = parts.queryItems?.first(where: { $0.name == "code" })?.value ?? ""
    guard isHTTPS, hasHost, isEnrollPath, !code.isEmpty else { return nil }

    // The API base is whatever host the QR came from, so it is never hardcoded.
    parts.path = ""
    parts.query = nil
    parts.fragment = nil
    guard let apiBase = parts.url else { return nil }
    return Invite(apiBase: apiBase, code: code)
}
