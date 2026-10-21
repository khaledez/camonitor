package net.khaledez.homeaccess

import java.net.InetAddress
import java.net.ServerSocket
import kotlin.concurrent.thread

/**
 * Minimal localhost HTTP server for GuestClient tests. It is a raw ServerSocket
 * because com.sun.net.httpserver is not visible to the Android-compiled test sources.
 */
class FakeHttpServer {
    @Volatile var status = 200
    @Volatile var responseBody = ""
    @Volatile var seenMethod = ""
    @Volatile var seenPath = ""
    @Volatile var seenAuth: String? = null
    @Volatile var seenRequestBody = ""

    private val socket = ServerSocket(0, 10, InetAddress.getByName("localhost"))
    val port get() = socket.localPort

    init {
        thread(isDaemon = true) {
            while (!socket.isClosed) {
                try { socket.accept().use { handle(it) } } catch (e: Exception) { /* closed or aborted */ }
            }
        }
    }

    private fun handle(conn: java.net.Socket) {
        val input = conn.getInputStream().bufferedReader(Charsets.ISO_8859_1)
        val (method, path) = input.readLine().split(" ")
        val headers = mutableMapOf<String, String>()
        while (true) {
            val line = input.readLine()
            if (line.isNullOrEmpty()) break
            val (name, value) = line.split(":", limit = 2)
            headers[name.trim().lowercase()] = value.trim()
        }
        val length = headers["content-length"]?.toInt() ?: 0
        val body = CharArray(length).also { input.read(it, 0, length) }.concatToString()

        seenMethod = method
        seenPath = path
        seenAuth = headers["authorization"]
        seenRequestBody = body

        val payload = responseBody.toByteArray()
        val head = "HTTP/1.1 $status X\r\nContent-Length: ${payload.size}\r\nConnection: close\r\n\r\n"
        conn.getOutputStream().apply { write(head.toByteArray()); write(payload); flush() }
    }

    fun stop() = socket.close()
}
