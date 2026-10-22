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

    private val _scanAttempt = MutableStateFlow(0)

    val phase: StateFlow<Phase> = _phase
    // The screen keys QrScanner on this; it fires once per instance, so a bump re-arms it.
    val scanAttempt: StateFlow<Int> = _scanAttempt
    val enrollError: StateFlow<String?> = _enrollError
    val actionStatus: StateFlow<String?> = _actionStatus
    val isBusy: StateFlow<Boolean> = _isBusy

    // Only a rejected code is remembered: the scanner keeps seeing the same QR, and
    // resubmitting a dead code is pointless, but a network failure must stay retryable.
    private var lastRejectedCode: String? = null

    // Runs on viewModelScope so a rotation mid-enroll can't cancel it between the server
    // consuming the one-time code and store.save.
    fun handleScanned(raw: String) {
        viewModelScope.launch {
            enroll(raw)
            if (_phase.value == Phase.NeedsEnrollment) {
                // Debounce so a foreign/dead QR in view doesn't rebind the camera in a tight loop.
                delay(1_000)
                _scanAttempt.value++
            }
        }
    }

    private suspend fun enroll(raw: String) {
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
            when (e.error) {
                GuestError.BadCode -> {
                    lastRejectedCode = invite.code
                    _enrollError.value = "Invite expired or already used."
                }
                GuestError.Network -> _enrollError.value = "Check your connection and try again."
                else -> _enrollError.value = "Something went wrong. Please try again."
            }
        }
    }

    fun openGate() = runAction { client, token -> client.openGate(token) }

    fun openOutdoor() = runAction { client, token -> client.openOutdoor(token) }

    private fun runAction(call: suspend (GuestApi, String) -> Unit) {
        if (_isBusy.value) return
        val session = store.load() ?: run {
            _phase.value = Phase.NeedsEnrollment
            return
        }
        _isBusy.value = true
        _actionStatus.value = null
        viewModelScope.launch {
            try {
                call(makeClient(session.apiBase), session.token)
            } catch (e: GuestException) {
                _isBusy.value = false
                when (e.error) {
                    GuestError.Unauthorized -> {
                        store.clear()
                        _phase.value = Phase.NeedsEnrollment
                    }
                    GuestError.DeviceUnavailable -> _actionStatus.value = "Device unavailable, try again"
                    else -> _actionStatus.value = "Couldn't open. Check your connection and try again"
                }
                return@launch
            }
            _isBusy.value = false
            _actionStatus.value = "Opened"
            delay(2_000)
            _actionStatus.value = null
        }
    }
}
