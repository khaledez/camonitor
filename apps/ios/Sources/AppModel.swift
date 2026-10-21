import Foundation

@MainActor
final class AppModel: ObservableObject {
    enum Phase { case needsEnrollment, enrolled }

    @Published var phase: Phase
    @Published var enrollError: String?
    @Published var actionStatus: String?
    @Published var actionSucceeded = false
    @Published var isOpening = false

    var makeClient: (URL) -> GuestClientProtocol = { GuestClient(apiBase: $0) }
    private var session: Session?
    private var client: GuestClientProtocol?

    init() {
        session = SessionStore.load()
        phase = session == nil ? .needsEnrollment : .enrolled
    }

    func handleScanned(_ raw: String) async {
        guard let invite = parseInviteQR(raw) else {
            enrollError = "That QR code is not a valid invite."
            return
        }
        let newClient = makeClient(invite.apiBase)
        do {
            let token = try await newClient.enroll(code: invite.code)
            let newSession = Session(apiBase: invite.apiBase, token: token)
            try SessionStore.save(newSession)
            session = newSession
            client = newClient
            enrollError = nil
            phase = .enrolled
        } catch GuestError.badCode {
            enrollError = "Invite expired or already used."
        } catch {
            enrollError = "Could not enroll. Check your connection and try again."
        }
    }

    func openGate() async {
        await open { try await $0.openGate(token: $1) }
    }

    func openOutdoor() async {
        await open { try await $0.openOutdoor(token: $1) }
    }

    private func open(_ action: (GuestClientProtocol, String) async throws -> Void) async {
        guard let session else { return }
        let activeClient = client ?? makeClient(session.apiBase)
        isOpening = true
        defer { isOpening = false }
        do {
            try await action(activeClient, session.token)
            actionSucceeded = true
            actionStatus = "Opened"
        } catch GuestError.unauthorized {
            SessionStore.clear()
            self.session = nil
            client = nil
            actionStatus = nil
            phase = .needsEnrollment
        } catch GuestError.deviceUnavailable {
            actionSucceeded = false
            actionStatus = "The door is not responding. Try again shortly."
        } catch {
            actionSucceeded = false
            actionStatus = "Could not reach the server. Try again."
        }
    }
}
