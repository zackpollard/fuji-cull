package pro.zackpollard.fujicull

import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.content.ServiceConnection
import android.hardware.usb.UsbManager
import android.os.Build
import android.os.Bundle
import android.os.IBinder
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import androidx.lifecycle.lifecycleScope
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import java.io.File

class MainActivity : ComponentActivity() {

    private var service by mutableStateOf<EngineService?>(null)
    private var usbDiag by mutableStateOf("scanning usb…")
    private val requested = mutableSetOf<String>()

    private val connection = object : ServiceConnection {
        override fun onServiceConnected(name: ComponentName?, binder: IBinder?) {
            service = (binder as EngineService.LocalBinder).service
            refreshUsb()
        }

        override fun onServiceDisconnected(name: ComponentName?) {
            service = null
        }
    }

    private val usbReceiver = object : BroadcastReceiver() {
        override fun onReceive(context: Context, intent: Intent) {
            if (intent.action == ACTION_USB_PERMISSION) requested.clear()
            refreshUsb()
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        startService(Intent(this, EngineService::class.java))
        bindService(Intent(this, EngineService::class.java), connection, Context.BIND_AUTO_CREATE)
        val filter = IntentFilter().apply {
            addAction(ACTION_USB_PERMISSION)
            addAction(UsbManager.ACTION_USB_DEVICE_ATTACHED)
            addAction(UsbManager.ACTION_USB_DEVICE_DETACHED)
        }
        if (Build.VERSION.SDK_INT >= 33) {
            registerReceiver(usbReceiver, filter, Context.RECEIVER_NOT_EXPORTED)
        } else {
            @Suppress("UnspecifiedRegisterReceiverFlag")
            registerReceiver(usbReceiver, filter)
        }
        // role swaps and mode changes don't always broadcast; keep the
        // diagnostics honest with a slow poll
        lifecycleScope.launch {
            while (true) {
                refreshUsb()
                delay(2000)
            }
        }

        setContent {
            CullApp(
                service = service,
                usbDiag = usbDiag,
                importDest = File(getExternalFilesDir(null), "import").absolutePath,
            )
        }
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        refreshUsb() // USB_DEVICE_ATTACHED relaunch
    }

    override fun onDestroy() {
        unregisterReceiver(usbReceiver)
        unbindService(connection)
        super.onDestroy()
    }

    /** Rebuilds the usb diagnostic line and attaches the camera when possible. */
    private fun refreshUsb() {
        val svc = service
        val usb = getSystemService(UsbManager::class.java)
        val devices = usb.deviceList.values.toList()
        val camera = devices.firstOrNull { it.vendorId == FUJI_VENDOR }
        usbDiag = when {
            devices.isEmpty() ->
                "usb: no devices visible — phone must be the usb host\n" +
                    "(usb notification → “controlled by: this device”)"
            camera == null ->
                "usb: " + devices.joinToString {
                    "%04x:%04x %s".format(it.vendorId, it.productId, it.productName ?: "?")
                } + "\n(no fujifilm device — set camera to usb card reader)"
            !usb.hasPermission(camera) ->
                "usb: ${camera.productName ?: "camera"} found — grant the permission prompt"
            svc?.usbAttached == true ->
                "usb: ${camera.productName ?: "camera"} attached · ${svc.claimDiag}"
            else ->
                "usb: ${camera.productName ?: "camera"} permitted — attaching…"
        }
        if (svc == null) return
        if (camera == null) {
            svc.detachUsb()
            return
        }
        if (!usb.hasPermission(camera)) {
            // one prompt per device until the broadcast answers, not one per poll
            if (requested.add(camera.deviceName)) {
                val pi = PendingIntent.getBroadcast(
                    this, 0,
                    Intent(ACTION_USB_PERMISSION).setPackage(packageName),
                    PendingIntent.FLAG_IMMUTABLE,
                )
                usb.requestPermission(camera, pi)
            }
            return
        }
        if (!svc.usbAttached) {
            usb.openDevice(camera)?.let { svc.attachUsb(camera, it) }
        }
    }

    companion object {
        const val ACTION_USB_PERMISSION = "pro.zackpollard.fujicull.USB_PERMISSION"
        const val FUJI_VENDOR = 0x04CB
    }
}
