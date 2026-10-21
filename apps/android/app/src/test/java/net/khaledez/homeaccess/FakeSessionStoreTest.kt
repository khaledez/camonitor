package net.khaledez.homeaccess

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class FakeSessionStoreTest {
    private val store = FakeSessionStore()

    @Test
    fun loadWhenEmptyReturnsNull() {
        assertNull(store.load())
    }

    @Test
    fun saveThenLoadRoundTrips() {
        val session = Session("https://home.example", "tok1")
        store.save(session)
        assertEquals(session, store.load())
    }

    @Test
    fun saveOverwritesPreviousSession() {
        store.save(Session("https://a.example", "old"))
        val newer = Session("https://b.example", "new")
        store.save(newer)
        assertEquals(newer, store.load())
    }

    @Test
    fun clearRemovesSession() {
        store.save(Session("https://a.example", "tok"))
        store.clear()
        assertNull(store.load())
    }
}
