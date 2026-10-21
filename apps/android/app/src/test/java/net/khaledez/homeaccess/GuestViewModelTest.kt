package net.khaledez.homeaccess

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.StandardTestDispatcher
import kotlinx.coroutines.test.advanceUntilIdle
import kotlinx.coroutines.test.runCurrent
import kotlinx.coroutines.test.resetMain
import kotlinx.coroutines.test.runTest
import kotlinx.coroutines.test.setMain
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Before
import org.junit.Test

private class FakeApi : GuestApi {
    var enrollError: GuestError? = null
    var actionError: GuestError? = null
    var enrollCalls = 0
    var lastToken: String? = null
    var lastAction: String? = null

    override suspend fun enroll(code: String): String {
        enrollCalls++
        enrollError?.let { throw GuestException(it) }
        return "tok"
    }

    override suspend fun openGate(token: String) {
        lastToken = token
        lastAction = "gate"
        actionError?.let { throw GuestException(it) }
    }

    override suspend fun openOutdoor(token: String) {
        lastToken = token
        lastAction = "outdoor"
        actionError?.let { throw GuestException(it) }
    }
}

@OptIn(ExperimentalCoroutinesApi::class)
class GuestViewModelTest {
    private val validQr = "https://home.example.com/guest/enroll?code=abc"
    private val store = FakeSessionStore()
    private val api = FakeApi()
    private val dispatcher = StandardTestDispatcher()

    @Before fun setUp() = Dispatchers.setMain(dispatcher)

    @After fun tearDown() = Dispatchers.resetMain()

    private fun newVm() = GuestViewModel(store) { api }

    private fun enrolledVm(): GuestViewModel {
        store.save(Session("https://home.example.com", "stored-token"))
        return newVm()
    }

    @Test fun startsNeedsEnrollmentWithoutSession() {
        assertEquals(Phase.NeedsEnrollment, newVm().phase.value)
    }

    @Test fun startsEnrolledWithSession() {
        assertEquals(Phase.Enrolled, enrolledVm().phase.value)
    }

    @Test fun validInviteEnrolls() = runTest(dispatcher) {
        val vm = newVm()
        vm.handleScanned(validQr)
        advanceUntilIdle()
        assertEquals(Phase.Enrolled, vm.phase.value)
        assertEquals(Session("https://home.example.com", "tok"), store.load())
    }

    @Test fun badCodeIsNotResubmitted() = runTest(dispatcher) {
        api.enrollError = GuestError.BadCode
        val vm = newVm()
        vm.handleScanned(validQr)
        advanceUntilIdle()
        vm.handleScanned(validQr)
        advanceUntilIdle()
        assertEquals(Phase.NeedsEnrollment, vm.phase.value)
        assertNotNull(vm.enrollError.value)
        assertEquals(1, api.enrollCalls)
    }

    @Test fun codeIsRetriedAfterNetworkFailure() = runTest(dispatcher) {
        api.enrollError = GuestError.Network
        val vm = newVm()
        vm.handleScanned(validQr)
        advanceUntilIdle()
        vm.handleScanned(validQr)
        advanceUntilIdle()
        assertEquals(2, api.enrollCalls)
        assertEquals(Phase.NeedsEnrollment, vm.phase.value)
        assertNotNull(vm.enrollError.value)
    }

    @Test fun foreignQrDoesNotEnroll() = runTest(dispatcher) {
        val vm = newVm()
        vm.handleScanned("https://example.com/other")
        advanceUntilIdle()
        assertEquals(0, api.enrollCalls)
        assertEquals(Phase.NeedsEnrollment, vm.phase.value)
        assertNotNull(vm.enrollError.value)
    }

    @Test fun unauthorizedGateClearsSession() = runTest(dispatcher) {
        api.actionError = GuestError.Unauthorized
        val vm = enrolledVm()
        vm.openGate()
        advanceUntilIdle()
        assertEquals(Phase.NeedsEnrollment, vm.phase.value)
        assertNull(store.load())
    }

    @Test fun deviceUnavailableShowsStatusAndStaysEnrolled() = runTest(dispatcher) {
        api.actionError = GuestError.DeviceUnavailable
        val vm = enrolledVm()
        vm.openGate()
        advanceUntilIdle()
        assertEquals(Phase.Enrolled, vm.phase.value)
        assertNotNull(vm.actionStatus.value)
        assertNotEquals("Opened", vm.actionStatus.value)
    }

    @Test fun successShowsStatusThenClears() = runTest(dispatcher) {
        val vm = enrolledVm()
        vm.openGate()
        runCurrent()
        assertEquals("Opened", vm.actionStatus.value)
        assertEquals(false, vm.isBusy.value)
        advanceUntilIdle()
        assertNull(vm.actionStatus.value)
    }

    @Test fun badCodeRearmsScannerAfterDelay() = runTest(dispatcher) {
        api.enrollError = GuestError.BadCode
        val vm = newVm()
        vm.handleScanned(validQr)
        runCurrent()
        assertEquals(0, vm.scanAttempt.value)
        advanceUntilIdle()
        assertEquals(1, vm.scanAttempt.value)
    }

    @Test fun actionsPassStoredToken() = runTest(dispatcher) {
        val vm = enrolledVm()
        vm.openGate()
        advanceUntilIdle()
        assertEquals("gate", api.lastAction)
        assertEquals("stored-token", api.lastToken)
        api.lastToken = null
        vm.openOutdoor()
        advanceUntilIdle()
        assertEquals("outdoor", api.lastAction)
        assertEquals("stored-token", api.lastToken)
    }
}
