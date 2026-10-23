import XCTest
@testable import HomeAccess

final class SessionStoreTests: XCTestCase {
    func testSaveLoadRoundTrip() throws {
        SessionStore.clear()
        let s = Session(apiBase: URL(string: "https://h.ts.net:8443")!, token: "abc123")
        try SessionStore.save(s)
        XCTAssertEqual(SessionStore.load(), s)
    }

    func testDecodesSessionSavedBeforeRedesign() throws {
        let old = Data(#"{"apiBase":"https://h.ts.net:8443","token":"t"}"#.utf8)
        let session = try JSONDecoder().decode(Session.self, from: old)
        XCTAssertEqual(session.token, "t")
        XCTAssertNil(session.label)
        XCTAssertFalse(session.isDemo)
    }

    func testClearRemoves() throws {
        try SessionStore.save(Session(apiBase: URL(string: "https://h")!, token: "t"))
        SessionStore.clear()
        XCTAssertNil(SessionStore.load())
    }

    func testOverwrite() throws {
        try SessionStore.save(Session(apiBase: URL(string: "https://h")!, token: "one"))
        try SessionStore.save(Session(apiBase: URL(string: "https://h")!, token: "two"))
        XCTAssertEqual(SessionStore.load()?.token, "two")
    }
}
