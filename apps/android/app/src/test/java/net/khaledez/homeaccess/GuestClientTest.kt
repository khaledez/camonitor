package net.khaledez.homeaccess

import kotlinx.coroutines.test.runTest
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.fail
import org.junit.Before
import org.junit.Test

class GuestClientTest {
    private lateinit var server: FakeHttpServer

    private val client get() = GuestClient("http://localhost:${server.port}")

    @Before fun start() { server = FakeHttpServer() }

    @After fun stop() = server.stop()

    private suspend fun assertFails(expected: GuestError, call: suspend () -> Unit) {
        try {
            call()
            fail("expected $expected")
        } catch (e: GuestException) {
            assertEquals(expected, e.error)
        }
    }

    @Test fun enrollReturnsToken() = runTest {
        server.responseBody = """{"token":"TOK"}"""
        assertEquals("TOK", client.enroll("C1"))
        assertEquals("POST", server.seenMethod)
        assertEquals("/guest/enroll", server.seenPath)
        assertEquals("""{"code":"C1"}""", server.seenRequestBody)
    }

    @Test fun enroll400IsBadCode() = runTest {
        server.status = 400
        assertFails(GuestError.BadCode) { client.enroll("C1") }
    }

    @Test fun enroll500IsServer() = runTest {
        server.status = 500
        assertFails(GuestError.Server) { client.enroll("C1") }
    }

    @Test fun gateOpenSendsBearerToken() = runTest {
        server.status = 204
        client.openGate("T")
        assertEquals("POST", server.seenMethod)
        assertEquals("/guest/gate/open", server.seenPath)
        assertEquals("Bearer T", server.seenAuth)
    }

    @Test fun outdoorOpenUsesOutdoorPath() = runTest {
        server.status = 204
        client.openOutdoor("T")
        assertEquals("/guest/outdoor/open", server.seenPath)
    }

    @Test fun open401IsUnauthorized() = runTest {
        server.status = 401
        assertFails(GuestError.Unauthorized) { client.openGate("T") }
    }

    @Test fun open502IsDeviceUnavailable() = runTest {
        server.status = 502
        assertFails(GuestError.DeviceUnavailable) { client.openOutdoor("T") }
    }

    @Test fun connectionRefusedIsNetwork() = runTest {
        val deadClient = client
        server.stop()
        assertFails(GuestError.Network) { deadClient.openGate("T") }
    }
}
