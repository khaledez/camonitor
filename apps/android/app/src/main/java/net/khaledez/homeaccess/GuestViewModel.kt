package net.khaledez.homeaccess

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.launch

enum class Phase { NeedsEnrollment, Enrolled }

class GuestViewModel(
    private val store: SessionStore,
    private val makeClient: (String) -> GuestApi = { GuestClient(it) },
) : ViewModel() {
    private val _phase = MutableStateFlow(if (store.load() != null) Phase.Enrolled else Phase.NeedsEnrollment)
    private val _enrollError = MutableStateFlow<String?>(null)
    private val _actionStatus = MutableStateFlow<String?>(null)
    private val _isBusy = MutableStateFlow(false)

    val phase: StateFlow<Phase> = _phase
    val enrollError: StateFlow<String?> = _enrollError
    val actionStatus: StateFlow<String?> = _actionStatus
    val isBusy: StateFlow<Boolean> = _isBusy

    // Only a rejected code is remembered: the scanner keeps seeing the same QR, and
    // resubmitting a dead code is pointless, but a network failure must stay retryable.
    private var lastRejectedCode: String? = null

    suspend fun handleScanned(raw: String) {
        val invite = parseInviteQR(raw)
        if (invite == null) {
            _enrollError.value = "That's not a valid invite"
            return
        }
        if (invite.code == lastRejectedCode) return

        try {
            val token = makeClient(invite.apiBase).enroll(invite.code)
            store.save(Session(invite.apiBase, token))
            lastRejectedCode = null
            _enrollError.value = null
            _phase.value = Phase.Enrolled
        } catch (e: GuestException) {
            if (e.error == GuestError.BadCode) {
                lastRejectedCode = invite.code
                _enrollError.value = "Invite expired or already used"
            } else {
                _enrollError.value = "Check your connection and try again"
            }
        }
    }

    fun openGate() = runAction { client, token -> client.openGate(token) }

    fun openOutdoor() = runAction { client, token -> client.openOutdoor(token) }

    private fun runAction(call: suspend (GuestApi, String) -> Unit) {
        val session = store.load() ?: run {
            _phase.value = Phase.NeedsEnrollment
            return
        }
        viewModelScope.launch {
            _isBusy.value = true
            _actionStatus.value = null
            try {
                call(makeClient(session.apiBase), session.token)
                _actionStatus.value = "Opened"
                delay(2_000)
                _actionStatus.value = null
            } catch (e: GuestException) {
                when (e.error) {
                    GuestError.Unauthorized -> {
                        store.clear()
                        _phase.value = Phase.NeedsEnrollment
                    }
                    GuestError.DeviceUnavailable -> _actionStatus.value = "Device unavailable, try again"
                    else -> _actionStatus.value = "Couldn't open. Check your connection and try again"
                }
            } finally {
                _isBusy.value = false
            }
        }
    }
}
