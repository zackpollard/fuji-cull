package pro.zackpollard.fujicull

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.Service
import android.content.Intent
import android.hardware.usb.UsbDeviceConnection
import android.os.Binder
import android.os.IBinder
import android.util.Log
import mobile.Engine
import mobile.Mobile
import java.io.File

/**
 * Foreground service owning the fuji-cull engine (Go core + loopback HTTP).
 * Holds the USB connection open for the engine's lifetime — closing the
 * UsbDeviceConnection would close the fd every camera session rides on.
 */
class EngineService : Service() {

    inner class LocalBinder : Binder() {
        val service: EngineService get() = this@EngineService
    }

    private val binder = LocalBinder()
    var engine: Engine? = null
        private set
    private var usb: UsbDeviceConnection? = null

    override fun onBind(intent: Intent?): IBinder = binder

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        startForeground(1, buildNotification())
        if (engine == null) {
            val dataDir = File(filesDir, "data").apply { mkdirs() }
            val cacheDir = File(cacheDir, "buffer").apply { mkdirs() }
            val aft = File(applicationInfo.nativeLibraryDir, "libaftcli.so")
            val prefs = getSharedPreferences("immich", MODE_PRIVATE)
            engine = Mobile.start(
                dataDir.absolutePath,
                cacheDir.absolutePath,
                if (aft.exists()) aft.absolutePath else "",
                prefs.getString("url", "") ?: "",
                prefs.getString("key", "") ?: "",
            )
            Log.i(TAG, "engine started on port ${engine?.port()}")
        }
        return START_STICKY
    }

    /** Hands a freshly-opened camera connection to the engine. */
    fun attachUsb(connection: UsbDeviceConnection) {
        usb?.close()
        usb = connection
        engine?.setUSBFD(connection.fileDescriptor.toLong())
        Log.i(TAG, "usb fd ${connection.fileDescriptor} attached")
    }

    override fun onDestroy() {
        engine?.stop()
        engine = null
        usb?.close()
        usb = null
        super.onDestroy()
    }

    private fun buildNotification(): Notification {
        val channel = NotificationChannel(
            CHANNEL, "fuji-cull engine", NotificationManager.IMPORTANCE_LOW
        )
        getSystemService(NotificationManager::class.java).createNotificationChannel(channel)
        return Notification.Builder(this, CHANNEL)
            .setContentTitle("fuji-cull")
            .setContentText("camera engine running")
            .setSmallIcon(android.R.drawable.ic_menu_camera)
            .build()
    }

    companion object {
        private const val TAG = "fuji-cull"
        private const val CHANNEL = "engine"
    }
}
