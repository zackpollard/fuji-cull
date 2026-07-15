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
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import androidx.lifecycle.lifecycleScope
import coil.Coil
import coil.ImageLoader
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import okhttp3.OkHttpClient
import java.io.File
import java.util.concurrent.TimeUnit

/** Engine settings persisted in prefs; changes restart the engine. */
data class Settings(
    val url: String = "",
    val key: String = "",
    val session: String = "",
    val stack: Boolean = false,
    val album: String = "",
)

class MainActivity : ComponentActivity() {

    private var service by mutableStateOf<EngineService?>(null)
    private var usbDiag by mutableStateOf("scanning usb…")
    private var resumeTick by mutableIntStateOf(0)
    private var engineEpoch by mutableIntStateOf(0)
    private var settings by mutableStateOf(Settings())
    private var lastRebuild = 0L
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
        // /api/image blocks until the shot is pulled off the camera — far
        // longer than Coil's default 10s read timeout on a busy link
        Coil.setImageLoader(
            ImageLoader.Builder(this)
                .okHttpClient(
                    OkHttpClient.Builder()
                        .readTimeout(180, TimeUnit.SECONDS)
                        // all images come from ONE loopback host; the default
                        // 5-requests-per-host cap made preloading crawl
                        .dispatcher(
                            okhttp3.Dispatcher().apply {
                                maxRequests = 64
                                maxRequestsPerHost = 32
                            },
                        )
                        .build(),
                )
                // scroll-ahead preloading keeps a few hundred thumbs warm
                .memoryCache { coil.memory.MemoryCache.Builder(this).maxSizePercent(0.30).build() }
                .build(),
        )
        settings = loadSettings()
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
        lifecycleScope.launch(kotlinx.coroutines.Dispatchers.Default) {
            while (true) {
                refreshUsb()
                delay(2000)
            }
        }

        setContent {
            CullApp(
                service = service,
                usbDiag = usbDiag,
                resumeTick = resumeTick,
                epoch = engineEpoch,
                importDest = File(getExternalFilesDir(null), "import").absolutePath,
                settings = settings,
                onSaveSettings = { s ->
                    saveSettings(s)
                    settings = s
                    service?.restartEngine()
                    engineEpoch++
                },
                onAlbumUsed = { album ->
                    prefs().edit().putString("album", album).apply()
                    settings = settings.copy(album = album)
                },
                onRescan = {
                    lifecycleScope.launch {
                        service?.engine?.let { runCatching { Api(it.port()).rescan() } }
                        service?.restartEngine()
                        engineEpoch++
                    }
                },
            )
        }
    }

    override fun onStart() {
        super.onStart()
        // returning from background: reprobe breakers immediately and let
        // Coil retry any cells that failed while the process was frozen
        resumeTick++
        runCatching { service?.engine?.nudge() }
        refreshUsb()
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

    private fun prefs() = getSharedPreferences("immich", MODE_PRIVATE)

    private fun loadSettings() = prefs().let {
        Settings(
            url = it.getString("url", "") ?: "",
            key = it.getString("key", "") ?: "",
            session = it.getString("session", "") ?: "",
            stack = it.getBoolean("stack", false),
            album = it.getString("album", "") ?: "",
        )
    }

    private fun saveSettings(s: Settings) {
        prefs().edit()
            .putString("url", s.url.trim().trimEnd('/'))
            .putString("key", s.key.trim())
            .putString("session", s.session.trim())
            .putBoolean("stack", s.stack)
            .putString("album", s.album)
            .apply()
    }

    /** Rebuilds the usb diagnostic line and attaches the camera when possible. */
    private fun refreshUsb() {
        val svc = service
        val usb = getSystemService(UsbManager::class.java)
        val devices = usb.deviceList.values.toList()
        val camera = devices.firstOrNull { it.vendorId == FUJI_VENDOR }
        usbDiag = when {
            devices.isEmpty() ->
                "usb: no devices visible — in the USB notification set\n" +
                    "“controlled by: connected device”"
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
            return
        }
        // link wedged despite a device reset: rebuild the connection from
        // this side (Android-level replug), at most once per half minute
        if (svc.engine?.linkDead() == true &&
            System.currentTimeMillis() - lastRebuild > 30_000
        ) {
            lastRebuild = System.currentTimeMillis()
            usb.openDevice(camera)?.let { svc.rebuildUsb(camera, it) }
        }
    }

    companion object {
        const val ACTION_USB_PERMISSION = "pro.zackpollard.fujicull.USB_PERMISSION"
        const val FUJI_VENDOR = 0x04CB
    }
}
