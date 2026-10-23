import Foundation

enum GuestError: Error, Equatable {
    case badCode, unauthorized, forbidden, deviceUnavailable, server, network
}

/// What enrolling returns. Only `token` is guaranteed: older servers sent nothing else.
struct Enrolled: Decodable, Equatable {
    let token: String
    var label: String? = nil
    var created: Int? = nil  // unix seconds
    var demo: Bool? = nil
}

struct GuestClient {
    let apiBase: URL
    var session: URLSession = .shared

    init(apiBase: URL, session: URLSession = .shared) {
        self.apiBase = apiBase
        self.session = session
    }

    func enroll(code: String) async throws -> Enrolled {
        var request = URLRequest(url: apiBase.appendingPathComponent("guest/enroll"))
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try JSONEncoder().encode(["code": code])

        let data = try await send(request)
        guard let enrolled = try? JSONDecoder().decode(Enrolled.self, from: data) else {
            throw GuestError.server
        }
        return enrolled
    }

    func openGate(token: String) async throws {
        _ = try await send(authorizedPost("guest/gate/open", token: token))
    }

    func openOutdoor(token: String) async throws {
        _ = try await send(authorizedPost("guest/outdoor/open", token: token))
    }

    /// Asks for a short-lived signed stream URL. It expires after ~15 minutes;
    /// the player asks again when playback fails.
    func cameraSession(camera: String, token: String) async throws -> URL {
        let data = try await send(authorizedPost("guest/cam/\(camera)/session", token: token))
        struct Reply: Decodable { let url: String }
        guard let reply = try? JSONDecoder().decode(Reply.self, from: data),
              let url = URL(string: reply.url, relativeTo: apiBase)?.absoluteURL else {
            throw GuestError.server
        }
        return url
    }

    func forget(token: String) async throws {
        _ = try await send(authorizedPost("guest/forget", token: token))
    }

    private func authorizedPost(_ path: String, token: String) -> URLRequest {
        var request = URLRequest(url: apiBase.appendingPathComponent(path))
        request.httpMethod = "POST"
        request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        return request
    }

    private func send(_ request: URLRequest) async throws -> Data {
        let data: Data
        let response: URLResponse
        do {
            (data, response) = try await session.data(for: request)
        } catch is URLError {
            throw GuestError.network
        }

        let status = (response as? HTTPURLResponse)?.statusCode ?? 0
        switch status {
        case 200..<300: return data
        case 400: throw GuestError.badCode
        case 401: throw GuestError.unauthorized
        case 403: throw GuestError.forbidden
        case 502: throw GuestError.deviceUnavailable
        default: throw GuestError.server
        }
    }
}

protocol GuestClientProtocol {
    func enroll(code: String) async throws -> Enrolled
    func openGate(token: String) async throws
    func openOutdoor(token: String) async throws
    func cameraSession(camera: String, token: String) async throws -> URL
    func forget(token: String) async throws
}

extension GuestClient: GuestClientProtocol {}
