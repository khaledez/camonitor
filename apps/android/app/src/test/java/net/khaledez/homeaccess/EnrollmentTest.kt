package net.khaledez.homeaccess

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class EnrollmentTest {
    @Test fun validInvite() {
        assertEquals(
            Invite("https://home.example.com", "ABC123"),
            parseInviteQR("https://home.example.com/guest/enroll?code=ABC123"),
        )
    }

    @Test fun stripsUserinfoAndKeepsPort() {
        assertEquals(
            Invite("https://h:8443", "X"),
            parseInviteQR("https://user:pass@h:8443/guest/enroll?code=X"),
        )
    }

    @Test fun rejectsWrongPath() {
        assertNull(parseInviteQR("https://h/other?code=X"))
    }

    @Test fun rejectsMissingOrEmptyCode() {
        assertNull(parseInviteQR("https://h/guest/enroll"))
        assertNull(parseInviteQR("https://h/guest/enroll?code="))
    }

    @Test fun rejectsNonUrl() {
        assertNull(parseInviteQR("not a url"))
    }

    @Test fun rejectsHttp() {
        assertNull(parseInviteQR("http://h/guest/enroll?code=X"))
    }
}
