package pro.zackpollard.fujicull

import android.content.Context
import android.graphics.Bitmap
import android.media.MediaMetadataRetriever
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext
import java.io.File

/**
 * Video poster frames. Android has no ffmpeg, so posters come from
 * MediaMetadataRetriever reading the engine's loopback stream — which opens
 * a camera streaming session per video, so extraction is strictly sequential
 * (concurrent sessions on different videos thrash the MTP claim) and results
 * are cached on disk forever (a shot's bytes never change).
 */
object Posters {
    private val lock = Mutex()

    fun cached(ctx: Context, shot: Shot): File? {
        val f = file(ctx, shot)
        return if (f.exists()) f else null
    }

    suspend fun load(ctx: Context, api: Api, shot: Shot): File? {
        val f = file(ctx, shot)
        if (f.exists()) return f
        return lock.withLock {
            if (f.exists()) return@withLock f
            withContext(Dispatchers.IO) {
                runCatching {
                    val mmr = MediaMetadataRetriever()
                    try {
                        mmr.setDataSource(api.videoUrl(shot.id), emptyMap())
                        val bmp = mmr.frameAtTime ?: return@runCatching null
                        val scaled = if (bmp.width > 480) {
                            Bitmap.createScaledBitmap(bmp, 480, bmp.height * 480 / bmp.width, true)
                        } else bmp
                        f.parentFile?.mkdirs()
                        val tmp = File(f.path + ".tmp")
                        tmp.outputStream().use { scaled.compress(Bitmap.CompressFormat.JPEG, 80, it) }
                        if (tmp.renameTo(f)) f else null
                    } finally {
                        mmr.release()
                    }
                }.getOrNull()
            }
        }
    }

    private fun file(ctx: Context, shot: Shot) =
        File(ctx.cacheDir, "posters/" + shot.id.replace('/', '_') + ".jpg")
}
