import XCTest
@testable import HomeAccess

final class EnrollmentTests: XCTestCase {
    func testParsesValidInvite() {
        let i = parseInviteQR("https://monitor.deer-ladon.ts.net:8443/guest/enroll?code=XYZ")
        XCTAssertEqual(i, Invite(apiBase: URL(string: "https://monitor.deer-ladon.ts.net:8443")!, code: "XYZ"))
    }
    func testRejectsWrongPath()   { XCTAssertNil(parseInviteQR("https://h.ts.net:8443/guest/gate/open?code=X")) }
    func testRejectsMissingCode() { XCTAssertNil(parseInviteQR("https://h.ts.net:8443/guest/enroll")) }
    func testRejectsEmptyCode()   { XCTAssertNil(parseInviteQR("https://h.ts.net:8443/guest/enroll?code=")) }
    func testRejectsNonURL()      { XCTAssertNil(parseInviteQR("hello world")) }
    func testRejectsHTTP()        { XCTAssertNil(parseInviteQR("http://h.ts.net/guest/enroll?code=X")) }
}
