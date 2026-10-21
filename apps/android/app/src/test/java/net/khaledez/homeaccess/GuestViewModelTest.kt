package net.khaledez.homeaccess

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.test.UnconfinedTestDispatcher
import kotlinx.coroutines.test.resetMain
import kotlinx.coroutines.test.runTest
import kotlinx.coroutines.test.setMain
import org.junit.After
import org.junit.Assert.assertEquals
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

    @Before fun setUp() = Dispatchers.setMain(UnconfinedTestDispatcher())

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

    @Test fun validInviteEnrolls() = runTest {
        val vm = newVm()
        vm.handleScanned(validQr)
        assertEquals(Phase.Enrolled, vm.phase.value)
        assertEquals(Session("https://home.example.com", "tok"), store.load())
    }

    @Test fun badCodeIsNotResubmitted() = runTest {
        api.enrollError = GuestError.BadCode
        val vm = newVm()
        vm.handleScanned(validQr)
        vm.handleScanned(validQr)
        assertEquals(Phase.NeedsEnrollment, vm.phase.value)
        assertNotNull(vm.enrollError.value)
        assertEquals(1, api.enrollCalls)
    }

    @Test fun codeIsRetriedAfterNetworkFailure() = runTest {
        api.enrollError = GuestError.Network
        val vm = newVm()
        vm.handleScanned(validQr)
        vm.handleScanned(validQr)
        assertEquals(2, api.enrollCalls)
        assertEquals(Phase.NeedsEnrollment, vm.phase.value)
        assertNotNull(vm.enrollError.value)
    }

    @Test fun foreignQrDoesNotEnroll() = runTest {
        val vm = newVm()
        vm.handleScanned("https://example.com/other")
        assertEquals(0, api.enrollCalls)
        assertEquals(Phase.NeedsEnrollment, vm.phase.value)
        assertNotNull(vm.enrollError.value)
    }

    @Test fun unauthorizedGateClearsSession() = runTest {
        api.actionError = GuestError.Unauthorized
        val vm = enrolledVm()
        vm.openGate()
        assertEquals(Phase.NeedsEnrollment, vm.phase.value)
        assertNull(store.load())
    }

    @Test fun deviceUnavailableShowsStatusAndStaysEnrolled() = runTest {
        api.actionError = GuestError.DeviceUnavailable
        val vm = enrolledVm()
        vm.openGate()
        assertEquals(Phase.Enrolled, vm.phase.value)
        assertNotNull(vm.actionStatus.value)
    }

    @Test fun actionsPassStoredToken() = runTest {
        val vm = enrolledVm()
        vm.openGate()
        assertEquals("gate", api.lastAction)
        assertEquals("stored-token", api.lastToken)
        api.lastToken = null
        vm.openOutdoor()
        assertEquals("outdoor", api.lastAction)
        assertEquals("stored-token", api.lastToken)
    }
}
