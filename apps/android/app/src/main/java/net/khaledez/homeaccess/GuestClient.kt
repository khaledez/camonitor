package net.khaledez.homeaccess

import java.io.IOException
import java.net.HttpURLConnection
import java.net.URL
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext

enum class GuestError { BadCode, Unauthorized, DeviceUnavailable, Server, Network }

/** Every GuestClient failure is thrown as this exception; read [error] to see which. */
class GuestException(val error: GuestError) : Exception(error.name)

private val TOKEN_FIELD = Regex(""""token"\s*:\s*"([^"]+)"""")

interface GuestApi {
    suspend fun enroll(code: String): String
    suspend fun openGate(token: String)
    suspend fun openOutdoor(token: String)
}

class GuestClient(val apiBase: String) : GuestApi {
    override suspend fun enroll(code: String): String {
        // org.json is a stub in JVM unit tests and we avoid a JSON dependency for two fields.
        val escapedCode = code.replace("\\", "\\\\").replace("\"", "\\\"")
        val response = post("/guest/enroll", body = """{"code":"$escapedCode"}""", token = null)
        val token = TOKEN_FIELD.find(response)?.groupValues?.get(1)
        return token ?: throw GuestException(GuestError.Server)
    }

    override suspend fun openGate(token: String) {
        post("/guest/gate/open", body = null, token = token)
    }

    override suspend fun openOutdoor(token: String) {
        post("/guest/outdoor/open", body = null, token = token)
    }

    private suspend fun post(path: String, body: String?, token: String?): String =
        withContext(Dispatchers.IO) {
            val conn = URL(apiBase + path).openConnection() as HttpURLConnection
            try {
                conn.requestMethod = "POST"
                conn.connectTimeout = 10_000
                conn.readTimeout = 10_000
                if (token != null) conn.setRequestProperty("Authorization", "Bearer $token")
                if (body != null) {
                    conn.setRequestProperty("Content-Type", "application/json")
                    conn.doOutput = true
                    conn.outputStream.use { it.write(body.toByteArray()) }
                }

                val status = conn.responseCode
                if (status !in 200..299) throw GuestException(errorFor(status))
                if (status == 204) "" else conn.inputStream.use { it.readBytes().decodeToString() }
            } catch (e: IOException) {
                throw GuestException(GuestError.Network)
            } finally {
                conn.disconnect()
            }
        }

    private fun errorFor(status: Int) = when (status) {
        400 -> GuestError.BadCode
        401 -> GuestError.Unauthorized
        502 -> GuestError.DeviceUnavailable
        else -> GuestError.Server
    }
}
