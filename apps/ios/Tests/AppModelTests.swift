import XCTest
@testable import HomeAccess

private final class FakeClient: GuestClientProtocol {
    var enrollResult: Result<Enrolled, GuestError> = .success(Enrolled(token: "tok", label: "Ahmed", created: 1761091200, demo: false))
    var openError: GuestError?
    var cameraError: GuestError?
    var forgetError: GuestError?
    var forgetCalls = 0
    var enrollCalls = 0
    var lastToken: String?
    var cameraSessionCalls = 0

    func enroll(code: String) async throws -> Enrolled {
        enrollCalls += 1
        return try enrollResult.get()
    }
    func openGate(token: String) async throws { lastToken = token; if let openError { throw openError } }
    func openOutdoor(token: String) async throws { lastToken = token; if let openError { throw openError } }
    func cameraSession(camera: String, token: String) async throws -> URL {
        cameraSessionCalls += 1
        if let cameraError { throw cameraError }
        return URL(string: "https://api.example.com/guest/cam/\(camera)/hls/index.m3u8?s=x")!
    }
    func forget(token: String) async throws {
        forgetCalls += 1
        if let forgetError { throw forgetError }
    }
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

    func testSameCodeIsRetriedAfterNetworkFailure() async {
        fake.enrollResult = .failure(.network)
        let model = makeModel()
        await model.handleScanned(validQR)
        await model.handleScanned(validQR)
        XCTAssertEqual(fake.enrollCalls, 2)
    }

    func testEnrollStoresGuestDetails() async {
        let model = makeModel()
        await model.handleScanned(validQR)
        XCTAssertEqual(model.session?.label, "Ahmed")
        XCTAssertEqual(model.session?.created, Date(timeIntervalSince1970: 1761091200))
        XCTAssertEqual(model.session?.isDemo, false)
    }

    func testDemoDoorRefusalKeepsEnrolledWithMessage() async {
        let model = makeModel()
        await model.handleScanned(validQR)
        fake.openError = .forbidden
        await model.openGate()
        XCTAssertEqual(model.phase, .enrolled)
        XCTAssertFalse(model.actionSucceeded)
        XCTAssertEqual(model.actionStatus, AppModel.demoRefusal)
    }

    func testForgetClearsSessionEvenWhenServerFails() async {
        let model = makeModel()
        await model.handleScanned(validQR)
        fake.forgetError = .network
        await model.forget()
        XCTAssertEqual(fake.forgetCalls, 1)
        XCTAssertEqual(model.phase, .needsEnrollment)
        XCTAssertNil(SessionStore.load())
        XCTAssertNil(model.session)
    }

    func testStreamURLReturnsSignedURL() async {
        let model = makeModel()
        await model.handleScanned(validQR)
        let url = await model.streamURL(camera: "15")
        XCTAssertEqual(url?.path, "/guest/cam/15/hls/index.m3u8")
    }

    func testStreamURLIsReusedWithinItsLifetime() async {
        let model = makeModel()
        await model.handleScanned(validQR)
        let first = await model.streamURL(camera: "10")
        let second = await model.streamURL(camera: "10")
        _ = await model.streamURL(camera: "15")
        XCTAssertEqual(first, second)
        XCTAssertEqual(fake.cameraSessionCalls, 2, "one session per camera, not per call")
    }

    func testStreamUnauthorizedResetsToEnroll() async {
        let model = makeModel()
        await model.handleScanned(validQR)
        fake.cameraError = .unauthorized
        let url = await model.streamURL(camera: "10")
        XCTAssertNil(url)
        XCTAssertEqual(model.phase, .needsEnrollment)
        XCTAssertNil(SessionStore.load())
    }
}
