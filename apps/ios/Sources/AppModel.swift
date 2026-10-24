import Foundation

@MainActor
final class AppModel: ObservableObject {
    enum Phase { case needsEnrollment, enrolled }

    static let demoRefusal = String(localized: "Opening is turned off for this demo pass.")

    @Published var phase: Phase
    @Published var enrollError: String?
    @Published var actionStatus: String?
    @Published var actionSucceeded = false
    @Published var isOpening = false
    @Published private(set) var session: Session?

    var makeClient: (URL) -> GuestClientProtocol = { GuestClient(apiBase: $0) }
    private var client: GuestClientProtocol?
    // Stops a rejected QR left in the camera frame from re-POSTing on every re-arm.
    private var lastRejectedCode: String?
    // Player retries and tile re-opens would otherwise each mint a session; the
    // signed URL stays valid for 15 minutes, so reuse it until just before expiry.
    private var cachedStreams: [String: (url: URL, fetchedAt: Date)] = [:]
    private static let streamReuseWindow: TimeInterval = 14 * 60

    init() {
        let saved = SessionStore.load()
        session = saved
        phase = saved == nil ? .needsEnrollment : .enrolled
    }

    /// Takes a scanned QR or a pasted invite link; both carry the same URL.
    func handleScanned(_ raw: String) async {
        guard let invite = parseInviteQR(raw) else {
            enrollError = String(localized: "That is not a valid invite.")
            return
        }
        if invite.code == lastRejectedCode { return }
        let newClient = makeClient(invite.apiBase)
        do {
            let enrolled = try await newClient.enroll(code: invite.code)
            let newSession = Session(
                apiBase: invite.apiBase,
                token: enrolled.token,
                label: enrolled.label,
                created: enrolled.created.map { Date(timeIntervalSince1970: TimeInterval($0)) },
                demo: enrolled.demo
            )
            try SessionStore.save(newSession)
            session = newSession
            client = newClient
            enrollError = nil
            lastRejectedCode = nil
            phase = .enrolled
        } catch GuestError.badCode {
            lastRejectedCode = invite.code
            enrollError = String(localized: "Invite expired or already used.")
        } catch {
            enrollError = String(localized: "Could not set up access. Check your connection and try again.")
        }
    }

    func openGate() async {
        await open { try await $0.openGate(token: $1) }
    }

    func openOutdoor() async {
        await open { try await $0.openOutdoor(token: $1) }
    }

    /// A fresh signed stream URL, or nil when the server can't give one.
    func streamURL(camera: String) async -> URL? {
        guard let session else { return nil }
        if let cached = cachedStreams[camera], Date().timeIntervalSince(cached.fetchedAt) < Self.streamReuseWindow {
            return cached.url
        }
        do {
            let url = try await activeClient(session).cameraSession(camera: camera, token: session.token)
            cachedStreams[camera] = (url, Date())
            return url
        } catch GuestError.unauthorized {
            resetToEnrollment()
            return nil
        } catch {
            return nil
        }
    }

    /// "Remove access from this phone": tell the server, then wipe the key no matter what.
    func forget() async {
        if let session {
            try? await activeClient(session).forget(token: session.token)
        }
        resetToEnrollment()
    }

    private func activeClient(_ session: Session) -> GuestClientProtocol {
        client ?? makeClient(session.apiBase)
    }

    private func resetToEnrollment() {
        SessionStore.clear()
        session = nil
        client = nil
        cachedStreams = [:]
        actionStatus = nil
        phase = .needsEnrollment
    }

    private func open(_ action: (GuestClientProtocol, String) async throws -> Void) async {
        guard let session else { return }
        isOpening = true
        defer { isOpening = false }
        do {
            try await action(activeClient(session), session.token)
            actionSucceeded = true
            actionStatus = String(localized: "Opened")
        } catch GuestError.unauthorized {
            resetToEnrollment()
        } catch GuestError.forbidden {
            actionSucceeded = false
            actionStatus = Self.demoRefusal
        } catch GuestError.deviceUnavailable {
            actionSucceeded = false
            actionStatus = String(localized: "The door is not responding. Try again shortly.")
        } catch {
            actionSucceeded = false
            actionStatus = String(localized: "Could not reach the server. Try again.")
        }
    }
}
