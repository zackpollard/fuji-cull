package pro.zackpollard.fujicull

import android.view.SurfaceHolder
import android.view.SurfaceView
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.Slider
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableFloatStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
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

/**
 * libmpv-backed player: ffmpeg software decode handles the 4:2:2 10-bit
 * HEVC that no Android MediaCodec touches, while hwdec=mediacodec still
 * covers ordinary clips with the hardware decoder. Streams the loopback
 * /api/video URL exactly like the desktop GUI's mpv does.
 *
 * MPVLib is process-global — only ever compose ONE of these at a time
 * (the viewer's active page enforces that).
 */
@Composable
fun MpvPlayer(api: Api, shot: Shot, modifier: Modifier = Modifier) {
    val context = LocalContext.current.applicationContext
    var paused by remember(shot.id) { mutableStateOf(false) }
    var duration by remember(shot.id) { mutableFloatStateOf(0f) }
    var position by remember(shot.id) { mutableFloatStateOf(0f) }
    var ready by remember(shot.id) { mutableStateOf(false) }
    var scrubbing by remember { mutableStateOf(false) }

    DisposableEffect(shot.id) {
        MPVLib.create(context)
        MPVLib.setOptionString("vo", "gpu")
        MPVLib.setOptionString("gpu-context", "android")
        MPVLib.setOptionString("opengl-es", "yes")
        // hardware decode where possible; ffmpeg software decode otherwise
        MPVLib.setOptionString("hwdec", "mediacodec")
        MPVLib.setOptionString("hwdec-codecs", "h264,hevc")
        MPVLib.setOptionString("ao", "audiotrack")
        MPVLib.setOptionString("vd-lavc-threads", "0")
        // 4K 4:2:2 sw decode is heavy on a phone: prefer dropping frames
        // over stalling playback
        MPVLib.setOptionString("vd-lavc-framedrop", "nonref")
        MPVLib.setOptionString("framedrop", "vo")
        MPVLib.setOptionString("cache", "yes")
        MPVLib.setOptionString("demuxer-max-bytes", "64MiB")
        MPVLib.setOptionString("demuxer-readahead-secs", "10")
        MPVLib.setOptionString("keep-open", "yes")
        MPVLib.init()
        onDispose {
            runCatching { MPVLib.destroy() }
            releaseCameraStream(api)
        }
    }

    LaunchedEffect(shot.id) {
        while (true) {
            runCatching {
                duration = MPVLib.getPropertyDouble("duration").toFloat()
                if (!scrubbing) position = MPVLib.getPropertyDouble("time-pos").toFloat()
                paused = MPVLib.getPropertyBoolean("pause")
                if (duration > 0f) ready = true
            }
            delay(400)
        }
    }

    Box(modifier.background(Color.Black)) {
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
            )
            Text(fmtTime(duration), color = Color.White, fontSize = 11.sp)
        }
        if (!ready) {
            CircularProgressIndicator(color = Amber, modifier = Modifier.align(Alignment.Center))
        }
        if (paused && ready) {
            Text("▶", color = Color.White, fontSize = 42.sp, modifier = Modifier.align(Alignment.Center))
        }
    }
}

private fun fmtTime(s: Float): String {
    val t = s.toInt().coerceAtLeast(0)
    return "%d:%02d".format(t / 60, t % 60)
}
