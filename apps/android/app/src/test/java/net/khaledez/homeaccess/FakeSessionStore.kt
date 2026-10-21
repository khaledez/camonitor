package net.khaledez.homeaccess

class FakeSessionStore : SessionStore {
    private var session: Session? = null

    override fun save(s: Session) {
        session = s
    }

    override fun load(): Session? = session

    override fun clear() {
        session = null
    }
}
