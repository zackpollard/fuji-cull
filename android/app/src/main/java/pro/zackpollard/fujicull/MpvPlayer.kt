package pro.zackpollard.fujicull

import android.view.SurfaceHolder
import android.view.SurfaceView
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Slider
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableFloatStateOf
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.ui.viewinterop.AndroidView
import androidx.compose.material3.Text
import dev.jdtech.mpv.MPVLib
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch

/**
 * libmpv-backed player: ffmpeg software decode handles the 4:2:2 10-bit
 * HEVC that no Android MediaCodec touches, while hwdec=mediacodec still
 * covers ordinary clips with the hardware decoder. Streams the loopback
 * /api/video URL exactly like the desktop GUI's mpv does.
 *
 * MPVLib is process-global — only ever compose ONE of these at a time
 * (the viewer's active page enforces that).
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun MpvPlayer(api: Api, shot: Shot, modifier: Modifier = Modifier) {
    val context = LocalContext.current.applicationContext
    val scope = rememberCoroutineScope()
    var paused by remember(shot.id) { mutableStateOf(false) }
    var duration by remember(shot.id) { mutableFloatStateOf(0f) }
    var position by remember(shot.id) { mutableFloatStateOf(0f) }
    var bufferedAhead by remember(shot.id) { mutableFloatStateOf(0f) }
    var buffering by remember(shot.id) { mutableStateOf(false) }
    var ready by remember(shot.id) { mutableStateOf(false) }
    var scrubbing by remember { mutableStateOf(false) }
    // hardware-decode wedge detector: consecutive polls where the stream is
    // buffered but the frame never advances (the 4:2:2 signature). Once it
    // trips we flip to software and stop checking.
    var wedgePolls by remember(shot.id) { mutableIntStateOf(0) }
    var softwareForced by remember(shot.id) { mutableStateOf(false) }

    DisposableEffect(shot.id) {
        // mpv's own diagnostics into the app log: remote debugging of
        // playback failures needs mpv's voice, not guesses
        var forwarded = 0
        val logObserver = MPVLib.LogObserver { prefix, level, text ->
            if (level <= 30 && forwarded < 25) { // warn and worse
                forwarded++
                scope.launch { api.logEvent("mpv[$prefix] ${text.trim()}") }
            }
        }
        MPVLib.create(context)
        MPVLib.addLogObserver(logObserver)
        MPVLib.setOptionString("vo", "gpu")
        MPVLib.setOptionString("gpu-context", "android")
        MPVLib.setOptionString("opengl-es", "yes")
        // DIRECT mediacodec (not -copy). -copy failed to build its internal
        // read-back surface ("native_window NULL") and dropped to software;
        // direct renders the decoder surface as a GL external texture, which
        // the gpu vo then letterboxes (correct aspect). 4:2:0 hardware-decodes
        // here (verified: ~20% of one core on 4K60).
        // Start on hardware. 4:2:0 (all recent footage) decodes here instantly.
        // The Tensor rejects 4:2:2 ("Unsupported profile") and mpv WON'T fall
        // back on its own — it just wedges black. The playback poll below
        // detects that wedge and flips hwdec to software at runtime (mpv keeps
        // the demuxer buffer, so no re-pull from the camera), which the gpu vo
        // then renders. No per-clip probing, no engine round-trip.
        MPVLib.setOptionString("hwdec", "mediacodec")
        // audiotrack tripped "Invalid audio buffer size" and lost sound;
        // opensles is the more forgiving Android ao.
        MPVLib.setOptionString("ao", "opensles")
        MPVLib.setOptionString("vd-lavc-threads", "0")
        // when we DO fall to software (4:2:2), make it as fast as possible so
        // it stays watchable for culling: skip the deblocking loop filter
        // (big HEVC speedup, minor quality loss — fine for review) and allow
        // fast/inexact decode; drop late frames rather than stall.
        MPVLib.setOptionString("profile", "fast")
        MPVLib.setOptionString("vd-lavc-fast", "yes")
        MPVLib.setOptionString("vd-lavc-skiploopfilter", "all")
        MPVLib.setOptionString("vd-lavc-framedrop", "nonref")
        MPVLib.setOptionString("framedrop", "vo")
        MPVLib.setOptionString("cache", "yes")
        // disk-backed demuxer cache: everything streamed this session stays
        // seekable (backward too) without re-pulling from the camera; the
        // temp file dies with the player
        MPVLib.setOptionString("cache-on-disk", "yes")
        MPVLib.setOptionString(
            "cache-dir",
            java.io.File(context.cacheDir, "mpvcache").apply { mkdirs() }.absolutePath,
        )
        MPVLib.setOptionString("demuxer-max-bytes", "256MiB")
        MPVLib.setOptionString("demuxer-max-back-bytes", "256MiB")
        MPVLib.setOptionString("demuxer-readahead-secs", "10")
        MPVLib.setOptionString("keep-open", "yes")
        MPVLib.init()
        scope.launch { api.logEvent("video open: ${shot.base}") }
        onDispose {
            runCatching { MPVLib.removeLogObserver(logObserver) }
            runCatching { MPVLib.destroy() }
            releaseCameraStream(api)
        }
    }

    LaunchedEffect(shot.id) {
        while (true) {
            // JNI property reads off the main thread
            data class Snap(val d: Float, val t: Float, val p: Boolean, val b: Float, val c: Boolean)
            val snap = kotlinx.coroutines.withContext(kotlinx.coroutines.Dispatchers.IO) {
                runCatching {
                    Snap(
                        MPVLib.getPropertyDouble("duration").toFloat(),
                        MPVLib.getPropertyDouble("time-pos").toFloat(),
                        MPVLib.getPropertyBoolean("pause"),
                        runCatching { MPVLib.getPropertyDouble("demuxer-cache-time").toFloat() }.getOrDefault(0f),
                        runCatching { MPVLib.getPropertyBoolean("paused-for-cache") }.getOrDefault(false),
                    )
                }.getOrNull()
            }
            if (snap != null) {
                duration = snap.d
                if (!scrubbing) position = snap.t
                paused = snap.p
                bufferedAhead = snap.b
                buffering = snap.c
                if (duration > 0f) ready = true

                // wedge detection: the file is open (duration known) and has
                // buffered real data, yet the frame clock hasn't left zero and
                // we're not paused or starved for cache — that's hardware
                // choking on 4:2:2. Give it a moment (poll is 400ms), then drop
                // to software on the already-buffered bytes.
                if (!softwareForced && !scrubbing && !snap.p && !snap.c &&
                    snap.d > 0f && snap.b > 0.5f && snap.t < 0.15f
                ) {
                    wedgePolls++
                    if (wedgePolls >= 6) {
                        softwareForced = true
                        kotlinx.coroutines.withContext(kotlinx.coroutines.Dispatchers.IO) {
                            runCatching {
                                MPVLib.setPropertyString("hwdec", "no")
                                MPVLib.command(arrayOf("seek", "0", "absolute"))
                            }
                        }
                        api.logEvent("hwdec: hardware wedged (likely 4:2:2) — switched to software")
                    }
                } else if (snap.t >= 0.15f) {
                    wedgePolls = 0
                }
            }
            delay(400)
        }
    }

    Box(modifier.fillMaxSize().background(Color.Black)) {
        AndroidView(
            factory = { ctx ->
                SurfaceView(ctx).apply {
                    holder.addCallback(object : SurfaceHolder.Callback {
                        override fun surfaceCreated(h: SurfaceHolder) {
                            runCatching {
                                MPVLib.attachSurface(h.surface)
                                MPVLib.setOptionString("force-window", "yes")
                                MPVLib.command(arrayOf("loadfile", api.videoUrl(shot.id)))
                            }
                        }

                        override fun surfaceChanged(h: SurfaceHolder, format: Int, w: Int, ht: Int) {
                            runCatching {
                                MPVLib.setPropertyString("android-surface-size", "${w}x$ht")
                            }
                        }

                        override fun surfaceDestroyed(h: SurfaceHolder) {
                            runCatching {
                                MPVLib.setOptionString("force-window", "no")
                                MPVLib.detachSurface()
                            }
                        }
                    })
                }
            },
            modifier = Modifier.fillMaxSize().clickable {
                runCatching { MPVLib.setPropertyBoolean("pause", !paused) }
            },
        )
        Row(
            Modifier.align(Alignment.BottomCenter).fillMaxWidth()
                .background(Color(0x99000000)).padding(horizontal = 12.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Text(fmtTime(position), color = Color.White, fontSize = 11.sp)
            Slider(
                value = position.coerceIn(0f, maxOf(duration, 0.1f)),
                onValueChange = {
                    scrubbing = true
                    position = it
                },
                onValueChangeFinished = {
                    runCatching { MPVLib.command(arrayOf("seek", position.toString(), "absolute")) }
                    scrubbing = false
                },
                valueRange = 0f..maxOf(duration, 0.1f),
                modifier = Modifier.weight(1f).padding(horizontal = 8.dp),
                track = {
                    // three segments: played (amber), buffered (dim gold),
                    // rest (dark) — the buffered edge is the visible signal
                    Box(Modifier.fillMaxWidth().height(5.dp)) {
                        Box(
                            Modifier.fillMaxSize()
                                .background(Color(0xFF33362F), RoundedCornerShape(3.dp)),
                        )
                        if (duration > 0f) {
                            Box(
                                Modifier
                                    .fillMaxWidth((bufferedAhead / duration).coerceIn(0f, 1f))
                                    .height(5.dp)
                                    .background(Color(0xFF9A7A35), RoundedCornerShape(3.dp)),
                            )
                            Box(
                                Modifier
                                    .fillMaxWidth((position / duration).coerceIn(0f, 1f))
                                    .height(5.dp)
                                    .background(Amber, RoundedCornerShape(3.dp)),
                            )
                        }
                    }
                },
            )
            Text(fmtTime(duration), color = Color.White, fontSize = 11.sp)
        }
        if (!ready || buffering) {
            CircularProgressIndicator(color = Amber, modifier = Modifier.align(Alignment.Center))
            if (buffering) {
                Text(
                    "buffering… ${fmtTime(bufferedAhead)} loaded",
                    color = Dim, fontSize = 11.sp,
                    modifier = Modifier.align(Alignment.Center).padding(top = 70.dp),
                )
            }
        }
        if (paused && ready && !buffering) {
            Text("▶", color = Color.White, fontSize = 42.sp, modifier = Modifier.align(Alignment.Center))
        }
    }
}

private fun fmtTime(s: Float): String {
    val t = s.toInt().coerceAtLeast(0)
    return "%d:%02d".format(t / 60, t % 60)
}
