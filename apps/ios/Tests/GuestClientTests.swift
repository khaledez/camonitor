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
            return (200, Data(#"{"token":"T","label":"Ahmed","created":1761091200,"demo":true}"#.utf8))
        }
        let enrolled = try await client.enroll(code: "C")
        XCTAssertEqual(enrolled.token, "T")
        XCTAssertEqual(enrolled.label, "Ahmed")
        XCTAssertEqual(enrolled.created, 1761091200)
        XCTAssertEqual(enrolled.demo, true)
    }

    func testEnrollAcceptsTokenOnlyReply() async throws {
        MockURLProtocol.handler = { _ in (200, Data(#"{"token":"T"}"#.utf8)) }
        let enrolled = try await client.enroll(code: "C")
        XCTAssertEqual(enrolled.token, "T")
        XCTAssertNil(enrolled.label)
    }

    func testForbiddenMapsTo403() async {
        MockURLProtocol.handler = { _ in (403, Data()) }
        await assertThrows(.forbidden) { try await self.client.openGate(token: "T") }
    }

    func testCameraSessionReturnsAbsoluteSignedURL() async throws {
        MockURLProtocol.handler = { req in
            XCTAssertEqual(req.httpMethod, "POST")
            XCTAssertEqual(req.url?.path, "/guest/cam/10/session")
            XCTAssertEqual(req.value(forHTTPHeaderField: "Authorization"), "Bearer T")
            return (200, Data(#"{"url":"/guest/cam/10/hls/index.m3u8?s=abc%2Bd","expires":1}"#.utf8))
        }
        let url = try await client.cameraSession(camera: "10", token: "T")
        XCTAssertEqual(url.absoluteString, "https://h.ts.net:8443/guest/cam/10/hls/index.m3u8?s=abc%2Bd")
    }

    func testForgetPostsWithBearer() async throws {
        MockURLProtocol.handler = { req in
            XCTAssertEqual(req.httpMethod, "POST")
            XCTAssertEqual(req.url?.path, "/guest/forget")
            XCTAssertEqual(req.value(forHTTPHeaderField: "Authorization"), "Bearer T")
            return (204, Data())
        }
        try await client.forget(token: "T")
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
