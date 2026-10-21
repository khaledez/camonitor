import XCTest
@testable import HomeAccess

final class MockURLProtocol: URLProtocol {
    static var handler: ((URLRequest) throws -> (Int, Data))?

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func stopLoading() {}
    override func startLoading() {
        do {
            let (status, body) = try MockURLProtocol.handler!(request)
            let response = HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: nil, headerFields: nil)!
            client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
            client?.urlProtocol(self, didLoad: body)
            client?.urlProtocolDidFinishLoading(self)
        } catch {
            client?.urlProtocol(self, didFailWithError: error)
        }
    }
}

final class GuestClientTests: XCTestCase {
    var client: GuestClient!

    override func setUp() {
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [MockURLProtocol.self]
        client = GuestClient(apiBase: URL(string: "https://h.ts.net:8443")!, session: URLSession(configuration: config))
    }

    func testEnrollReturnsToken() async throws {
        MockURLProtocol.handler = { req in
            XCTAssertEqual(req.httpMethod, "POST")
            XCTAssertEqual(req.url?.path, "/guest/enroll")
            return (200, Data(#"{"token":"T"}"#.utf8))
        }
        let token = try await client.enroll(code: "C")
        XCTAssertEqual(token, "T")
    }

    func testEnrollBadCodeThrows() async {
        MockURLProtocol.handler = { _ in (400, Data()) }
        await assertThrows(.badCode) { _ = try await self.client.enroll(code: "C") }
    }

    func testEnrollNetworkError() async {
        MockURLProtocol.handler = { _ in throw URLError(.notConnectedToInternet) }
        await assertThrows(.network) { _ = try await self.client.enroll(code: "C") }
    }

    func testOpenGateSuccess() async throws {
        MockURLProtocol.handler = { req in
            XCTAssertEqual(req.httpMethod, "POST")
            XCTAssertEqual(req.url?.path, "/guest/gate/open")
            XCTAssertEqual(req.value(forHTTPHeaderField: "Authorization"), "Bearer T")
            return (204, Data())
        }
        try await client.openGate(token: "T")
    }

    func testOpenGateUnauthorized() async {
        MockURLProtocol.handler = { _ in (401, Data()) }
        await assertThrows(.unauthorized) { try await self.client.openGate(token: "T") }
    }

    func testOpenGateDeviceUnavailable() async {
        MockURLProtocol.handler = { _ in (502, Data()) }
        await assertThrows(.deviceUnavailable) { try await self.client.openGate(token: "T") }
    }

    func testOpenOutdoorSuccess() async throws {
        MockURLProtocol.handler = { req in
            XCTAssertEqual(req.url?.path, "/guest/outdoor/open")
            XCTAssertEqual(req.value(forHTTPHeaderField: "Authorization"), "Bearer T")
            return (204, Data())
        }
        try await client.openOutdoor(token: "T")
    }

    private func assertThrows(_ expected: GuestError, _ body: () async throws -> Void) async {
        do {
            try await body()
            XCTFail("expected \(expected)")
        } catch {
            XCTAssertEqual(error as? GuestError, expected)
        }
    }
}
