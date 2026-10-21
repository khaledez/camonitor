import XCTest
@testable import HomeAccess

private final class FakeClient: GuestClientProtocol {
    var enrollResult: Result<String, GuestError> = .success("tok")
    var openError: GuestError?
    var enrollCalls = 0
    var lastToken: String?

    func enroll(code: String) async throws -> String {
        enrollCalls += 1
        return try enrollResult.get()
    }
    func openGate(token: String) async throws { lastToken = token; if let openError { throw openError } }
    func openOutdoor(token: String) async throws { lastToken = token; if let openError { throw openError } }
}

@MainActor
final class AppModelTests: XCTestCase {
    private let validQR = "https://api.example.com/guest/enroll?code=abc"
    private var fake = FakeClient()

    override func setUp() {
        SessionStore.clear()
        fake = FakeClient()
    }
    override func tearDown() { SessionStore.clear() }

    private func makeModel() -> AppModel {
        let model = AppModel()
        model.makeClient = { [fake] _ in fake }
        return model
    }

    func testStartsNeedsEnrollmentWhenNoSession() {
        XCTAssertEqual(makeModel().phase, .needsEnrollment)
    }

    func testScanValidInviteEnrollsAndAdvances() async {
        let model = makeModel()
        await model.handleScanned(validQR)
        XCTAssertEqual(model.phase, .enrolled)
        XCTAssertEqual(SessionStore.load()?.token, "tok")
        XCTAssertEqual(SessionStore.load()?.apiBase.host, "api.example.com")
    }

    func testScanBadCodeShowsErrorStaysOnEnroll() async {
        fake.enrollResult = .failure(.badCode)
        let model = makeModel()
        await model.handleScanned(validQR)
        XCTAssertEqual(model.phase, .needsEnrollment)
        XCTAssertNotNil(model.enrollError)
        XCTAssertNil(SessionStore.load())
    }

    func testScanForeignQRIgnored() async {
        let model = makeModel()
        await model.handleScanned("https://evil.example/hello")
        XCTAssertEqual(fake.enrollCalls, 0)
        XCTAssertEqual(model.phase, .needsEnrollment)
        XCTAssertNotNil(model.enrollError)
    }

    func testOpenGateUnauthorizedResetsToEnroll() async {
        let model = makeModel()
        await model.handleScanned(validQR)
        fake.openError = .unauthorized
        await model.openGate()
        XCTAssertEqual(model.phase, .needsEnrollment)
        XCTAssertNil(SessionStore.load())
    }

    func testOpenGateDeviceUnavailableKeepsEnrolled() async {
        let model = makeModel()
        await model.handleScanned(validQR)
        fake.openError = .deviceUnavailable
        await model.openGate()
        XCTAssertEqual(model.phase, .enrolled)
        XCTAssertNotNil(model.actionStatus)
        XCTAssertFalse(model.actionSucceeded)
    }

    func testSameRejectedCodeIsNotResubmitted() async {
        fake.enrollResult = .failure(.badCode)
        let model = makeModel()
        await model.handleScanned(validQR)
        await model.handleScanned(validQR)
        XCTAssertEqual(fake.enrollCalls, 1)
        XCTAssertEqual(model.phase, .needsEnrollment)
    }

    func testDifferentCodeAfterRejectionIsSubmitted() async {
        fake.enrollResult = .failure(.badCode)
        let model = makeModel()
        await model.handleScanned(validQR)
        await model.handleScanned("https://api.example.com/guest/enroll?code=other")
        XCTAssertEqual(fake.enrollCalls, 2)
    }

    func testOpenCallsReceiveStoredToken() async {
        let model = makeModel()
        await model.handleScanned(validQR)
        await model.openGate()
        XCTAssertEqual(fake.lastToken, "tok")
        fake.lastToken = nil
        await model.openOutdoor()
        XCTAssertEqual(fake.lastToken, "tok")
    }

    func testOpenOutdoorSuccessKeepsEnrolledAndSetsStatus() async {
        let model = makeModel()
        await model.handleScanned(validQR)
        await model.openOutdoor()
        XCTAssertEqual(model.phase, .enrolled)
        XCTAssertTrue(model.actionSucceeded)
        XCTAssertNotNil(model.actionStatus)
    }
}
