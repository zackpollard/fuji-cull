package pro.zackpollard.fujicull

import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.content.ServiceConnection
import android.hardware.usb.UsbDevice
import android.hardware.usb.UsbManager
import android.os.Build
import android.os.Bundle
import android.os.IBinder
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import java.io.File

class MainActivity : ComponentActivity() {

    private var service by mutableStateOf<EngineService?>(null)

    private val connection = object : ServiceConnection {
        override fun onServiceConnected(name: ComponentName?, binder: IBinder?) {
            service = (binder as EngineService.LocalBinder).service
            attachCameraIfPermitted()
        }

        override fun onServiceDisconnected(name: ComponentName?) {
            service = null
        }
    }

    private val permissionReceiver = object : BroadcastReceiver() {
        override fun onReceive(context: Context, intent: Intent) {
            if (intent.action == ACTION_USB_PERMISSION) attachCameraIfPermitted()
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        startForegroundService(Intent(this, EngineService::class.java))
        bindService(Intent(this, EngineService::class.java), connection, Context.BIND_AUTO_CREATE)
        val filter = IntentFilter(ACTION_USB_PERMISSION)
        if (Build.VERSION.SDK_INT >= 33) {
            registerReceiver(permissionReceiver, filter, Context.RECEIVER_NOT_EXPORTED)
        } else {
            @Suppress("UnspecifiedRegisterReceiverFlag")
            registerReceiver(permissionReceiver, filter)
        }

        setContent {
            CullApp(
                service = service,
                importDest = File(getExternalFilesDir(null), "import").absolutePath,
            )
        }
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        attachCameraIfPermitted() // USB_DEVICE_ATTACHED relaunch
    }

    override fun onDestroy() {
        unregisterReceiver(permissionReceiver)
        unbindService(connection)
        super.onDestroy()
    }

    /** Finds a Fuji camera, requests permission if needed, hands the fd over. */
    private fun attachCameraIfPermitted() {
        val svc = service ?: return
        val usb = getSystemService(UsbManager::class.java)
        val camera: UsbDevice = usb.deviceList.values.firstOrNull { it.vendorId == 0x04CB }
            ?: return
        if (!usb.hasPermission(camera)) {
            val pi = PendingIntent.getBroadcast(
                this, 0,
                Intent(ACTION_USB_PERMISSION).setPackage(packageName),
                PendingIntent.FLAG_IMMUTABLE,
            )
            usb.requestPermission(camera, pi)
            return
        }
        usb.openDevice(camera)?.let { svc.attachUsb(it) }
    }

    companion object {
        const val ACTION_USB_PERMISSION = "pro.zackpollard.fujicull.USB_PERMISSION"
    }
}
