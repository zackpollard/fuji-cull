package pro.zackpollard.fujicull

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.Service
import android.content.Intent
import android.content.pm.ServiceInfo
import android.hardware.usb.UsbDevice
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
    var startError: String? = null
        private set
    private var usb: UsbDeviceConnection? = null
    val usbAttached: Boolean get() = usb != null
    var claimDiag: String = ""
        private set

    override fun onBind(intent: Intent?): IBinder = binder

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (engine == null) {
            try {
                val dataDir = File(filesDir, "data").apply { mkdirs() }
                val bufDir = File(cacheDir, "buffer").apply { mkdirs() }
                val aft = File(applicationInfo.nativeLibraryDir, "libaftcli.so")
                val prefs = getSharedPreferences("immich", MODE_PRIVATE)
                engine = Mobile.start(
                    dataDir.absolutePath,
                    bufDir.absolutePath,
                    if (aft.exists()) aft.absolutePath else "",
                    prefs.getString("url", "") ?: "",
                    prefs.getString("key", "") ?: "",
                )
                startError = null
                Log.i(TAG, "engine started on port ${engine?.port()}")
            } catch (t: Throwable) {
                startError = t.message ?: t.toString()
                Log.e(TAG, "engine start failed", t)
            }
        }
        return START_STICKY
    }

    /** Hands a freshly-opened camera connection to the engine. */
    fun attachUsb(device: UsbDevice, connection: UsbDeviceConnection) {
        usb?.close()
        usb = connection
        // Android's built-in MTP host grabs PTP devices on attach; force-claim
        // steals the interface back. aft rides the same fd, so this claim is
        // its claim too.
        claimDiag = (0 until device.interfaceCount).joinToString(" ") { i ->
            val intf = device.getInterface(i)
            val ok = connection.claimInterface(intf, true)
            "intf$i(class ${intf.interfaceClass})=${if (ok) "claimed" else "BUSY"}"
        }
        Log.i(TAG, "usb fd ${connection.fileDescriptor} attached: $claimDiag")
        engine?.setUSBFD(connection.fileDescriptor.toLong())
        // connectedDevice FGS is only permitted while we hold a USB device
        // grant, so promotion has to wait until a camera is attached
        try {
            startForeground(1, buildNotification(), ServiceInfo.FOREGROUND_SERVICE_TYPE_CONNECTED_DEVICE)
        } catch (t: Throwable) {
            Log.w(TAG, "foreground promotion failed, staying background", t)
        }
    }

    /** Drops the connection once the platform reports the device gone. */
    fun detachUsb() {
        if (usb == null) return
        engine?.clearUSBFD()
        usb?.close()
        usb = null
        claimDiag = ""
        Log.i(TAG, "usb detached")
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
