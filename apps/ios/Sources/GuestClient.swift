import Foundation

enum GuestError: Error, Equatable {
    case badCode, unauthorized, deviceUnavailable, server, network
}

struct GuestClient {
    let apiBase: URL
    var session: URLSession = .shared

    init(apiBase: URL, session: URLSession = .shared) {
        self.apiBase = apiBase
        self.session = session
    }

    func enroll(code: String) async throws -> String {
        var request = URLRequest(url: apiBase.appendingPathComponent("guest/enroll"))
        request.httpMethod = "POST"
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try JSONEncoder().encode(["code": code])

        let data = try await send(request)
        guard let token = try? JSONDecoder().decode([String: String].self, from: data)["token"] else {
            throw GuestError.server
        }
        return token
    }

    func openGate(token: String) async throws {
        _ = try await send(authorizedPost("guest/gate/open", token: token))
    }

    func openOutdoor(token: String) async throws {
        _ = try await send(authorizedPost("guest/outdoor/open", token: token))
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
        case 502: throw GuestError.deviceUnavailable
        default: throw GuestError.server
        }
    }
}
