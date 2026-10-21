package net.khaledez.homeaccess

import android.content.Context
import androidx.security.crypto.EncryptedSharedPreferences
import androidx.security.crypto.MasterKey

data class Session(val apiBase: String, val token: String)

interface SessionStore {
    fun save(s: Session)
    fun load(): Session?
    fun clear()
}

// The token lives only in Keystore-backed encrypted prefs; never log it.
class KeystoreSessionStore(context: Context) : SessionStore {
    private val prefs = EncryptedSharedPreferences.create(
        context,
        "session",
        MasterKey.Builder(context).setKeyScheme(MasterKey.KeyScheme.AES256_GCM).build(),
        EncryptedSharedPreferences.PrefKeyEncryptionScheme.AES256_SIV,
        EncryptedSharedPreferences.PrefValueEncryptionScheme.AES256_GCM,
    )

    override fun save(s: Session) {
        prefs.edit().putString("apiBase", s.apiBase).putString("token", s.token).apply()
    }

    override fun load(): Session? {
        val apiBase = prefs.getString("apiBase", null) ?: return null
        val token = prefs.getString("token", null) ?: return null
        return Session(apiBase, token)
    }

    override fun clear() {
        prefs.edit().clear().apply()
    }
}
