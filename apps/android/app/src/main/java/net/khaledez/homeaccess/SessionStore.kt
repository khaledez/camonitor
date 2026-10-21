package net.khaledez.homeaccess

import android.content.Context
import android.content.SharedPreferences
import androidx.security.crypto.EncryptedSharedPreferences
import androidx.security.crypto.MasterKey

data class Session(val apiBase: String, val token: String)

interface SessionStore {
    fun save(s: Session)
    fun load(): Session?
    fun clear()
}

// The token lives only in Keystore-backed encrypted prefs; never log it.
class KeystoreSessionStore(private val context: Context) : SessionStore {
    private val prefsName = "session"

    private fun openPrefs() = EncryptedSharedPreferences.create(
        context,
        prefsName,
        MasterKey.Builder(context).setKeyScheme(MasterKey.KeyScheme.AES256_GCM).build(),
        EncryptedSharedPreferences.PrefKeyEncryptionScheme.AES256_SIV,
        EncryptedSharedPreferences.PrefValueEncryptionScheme.AES256_GCM,
    )

    // An invalidated Keystore key (lock-screen reset, restore) makes the old prefs
    // undecryptable; wipe them so the guest just has to sign in again.
    private fun <T> withPrefs(fallback: T, block: (SharedPreferences) -> T): T {
        return try {
            block(openPrefs())
        } catch (e: Exception) {
            context.deleteSharedPreferences(prefsName)
            fallback
        }
    }

    override fun save(s: Session) = withPrefs(Unit) {
        it.edit().putString("apiBase", s.apiBase).putString("token", s.token).apply()
    }

    override fun load(): Session? = withPrefs(null) {
        val apiBase = it.getString("apiBase", null) ?: return@withPrefs null
        val token = it.getString("token", null) ?: return@withPrefs null
        Session(apiBase, token)
    }

    override fun clear() = withPrefs(Unit) { it.edit().clear().apply() }
}
