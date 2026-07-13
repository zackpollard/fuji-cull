package pro.zackpollard.fujicull

import android.content.Context
import android.graphics.Bitmap
import android.media.MediaMetadataRetriever
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext
import java.io.File
import java.net.HttpURLConnection
import java.net.URL

/**
 * Video poster frames. Android has no ffmpeg, so posters come from
 * MediaMetadataRetriever — but NEVER over the network: MMR parsing a
 * multi-GB HEVC across the loopback stream can wedge for minutes while
 * holding the camera's streaming session (observed in the field: the same
 * 521 MB video reopening in a loop). Instead the first 8 MB is downloaded
 * (Fuji writes moov + the opening frames at the head — the same slice
 * desktop ffmpeg posters use), the camera is released immediately, and MMR
 * parses the local file offline. Failures are cached as markers so a video
 * MMR can't decode is attempted exactly once.
 */
object Posters {
    private val lock = Mutex()
    private const val HEAD_BYTES = 8L * 1024 * 1024

    fun cached(ctx: Context, shot: Shot): File? {
        val f = file(ctx, shot)
        return if (f.exists()) f else null
    }

    suspend fun load(ctx: Context, api: Api, shot: Shot): File? {
        val f = file(ctx, shot)
        if (f.exists()) return f
        // .fail2: fresh namespace — markers from older extraction logic
        // shouldn't permanently doom a video
        val fail = File(f.path + ".fail2")
        if (fail.exists()) return null
        return lock.withLock {
            if (f.exists()) return@withLock f
            if (fail.exists()) return@withLock null
            withContext(Dispatchers.IO) {
                val head = File(ctx.cacheDir, "posters/${f.nameWithoutExtension}.head")
                // /api/videohead rides the shared partial-read session —
                // no streaming claim, no readahead, exactly 8 MB. A 503
                // means the link is busy (streaming/import): transient,
                // do NOT mark the video failed.
                val fetched = runCatching {
                    val c = URL(api.videoHeadUrl(shot.id)).openConnection() as HttpURLConnection
                    c.connectTimeout = 5000
                    c.readTimeout = 120000
                    if (c.responseCode != 200) return@runCatching false
                    head.parentFile?.mkdirs()
                    c.inputStream.use { ins -> head.outputStream().use { ins.copyTo(it) } }
                    head.length() > 0
                }.getOrDefault(false)
                if (!fetched) {
                    head.delete()
                    return@withContext null // transient: retried on a later recomposition
                }

                var diag = ""
                runCatching { patchTruncatedBoxes(head) }
                val bmp: Bitmap? = runCatching {
                    val mmr = MediaMetadataRetriever()
                    try {
                        mmr.setDataSource(head.absolutePath)
                        diag = "mime=" + mmr.extractMetadata(MediaMetadataRetriever.METADATA_KEY_MIMETYPE) +
                            " w=" + mmr.extractMetadata(MediaMetadataRetriever.METADATA_KEY_VIDEO_WIDTH)
                        // frame 0 explicitly: the no-arg "representative
                        // frame" often seeks mid-video, past the head
                        mmr.getFrameAtTime(0, MediaMetadataRetriever.OPTION_CLOSEST_SYNC)
                    } finally {
                        mmr.release()
                    }
                }.onFailure { diag += " err=${it.message}" }.getOrNull()
                head.delete()

                if (bmp == null) {
                    api.logEvent("poster: ${shot.base} undecodable ($diag; marked, no retry)")
                    fail.parentFile?.mkdirs()
                    fail.writeText("")
                    null
                } else {
                    val scaled = if (bmp.width > 480) {
                        Bitmap.createScaledBitmap(bmp, 480, bmp.height * 480 / bmp.width, true)
                    } else bmp
                    val tmp = File(f.path + ".tmp")
                    tmp.outputStream().use { scaled.compress(Bitmap.CompressFormat.JPEG, 80, it) }
                    if (tmp.renameTo(f)) {
                        api.logEvent("poster: ${shot.base} ok")
                        f
                    } else null
                }
            }
        }
    }

    /**
     * Rewrites the size of the box the download truncated (mdat, since Fuji
     * writes moov first) so the container is self-consistent — Android's
     * extractor rejects boxes that claim more bytes than the file holds,
     * where ffmpeg would just read what's there.
     */
    private fun patchTruncatedBoxes(f: File) {
        java.io.RandomAccessFile(f, "rw").use { raf ->
            val len = raf.length()
            var off = 0L
            while (off + 8 <= len) {
                raf.seek(off)
                var size = raf.readInt().toLong() and 0xFFFFFFFFL
                raf.skipBytes(4) // box type
                var header = 8L
                if (size == 1L) {
                    size = raf.readLong()
                    header = 16L
                }
                if (size == 0L) size = len - off // "to end of file"
                if (off + size > len) {
                    if (header == 8L) {
                        raf.seek(off)
                        raf.writeInt((len - off).toInt())
                    } else {
                        raf.seek(off + 8)
                        raf.writeLong(len - off)
                    }
                    return
                }
                off += size
            }
        }
    }

    private fun file(ctx: Context, shot: Shot) =
        File(ctx.cacheDir, "posters/" + shot.id.replace('/', '_') + ".jpg")
}
