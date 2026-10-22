package net.khaledez.homeaccess

import java.net.URI
import java.net.URLDecoder

data class Invite(val apiBase: String, val code: String)

/** Parses `https://<host>/guest/enroll?code=<code>`; returns null if any rule fails. */
fun parseInviteQR(raw: String): Invite? {
    val uri = try { URI(raw.trim()) } catch (e: Exception) { return null }

    val host = uri.host
    if (uri.scheme != "https" || host.isNullOrEmpty()) return null
    if (uri.path?.endsWith("/guest/enroll") != true) return null

    val code = uri.rawQuery
        ?.split("&")
        ?.firstOrNull { it.startsWith("code=") }
        ?.removePrefix("code=")
        ?.let { URLDecoder.decode(it, "UTF-8") }
    if (code.isNullOrEmpty()) return null

    val port = if (uri.port == -1) "" else ":${uri.port}"
    return Invite(apiBase = "https://$host$port", code = code)
}
