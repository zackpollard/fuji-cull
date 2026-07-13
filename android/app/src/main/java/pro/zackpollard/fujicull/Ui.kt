package pro.zackpollard.fujicull

import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.gestures.awaitEachGesture
import androidx.compose.foundation.gestures.awaitFirstDown
import androidx.compose.foundation.gestures.calculateCentroid
import androidx.compose.foundation.gestures.calculatePan
import androidx.compose.foundation.gestures.calculateZoom
import androidx.compose.foundation.gestures.detectTapGestures
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.aspectRatio
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.safeDrawingPadding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyRow
import androidx.compose.foundation.lazy.itemsIndexed as listItemsIndexed
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.lazy.grid.GridCells
import androidx.compose.foundation.lazy.grid.LazyVerticalGrid
import androidx.compose.foundation.lazy.grid.itemsIndexed
import androidx.compose.foundation.lazy.grid.rememberLazyGridState
import androidx.compose.foundation.pager.HorizontalPager
import androidx.compose.foundation.pager.rememberPagerState
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.darkColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.key
import androidx.compose.runtime.mutableFloatStateOf
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.produceState
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.runtime.snapshotFlow
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.geometry.isSpecified
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.graphicsLayer
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.layout.onSizeChanged
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.IntSize
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.ui.viewinterop.AndroidView
import androidx.activity.compose.BackHandler
import androidx.compose.ui.window.Dialog
import androidx.media3.common.MediaItem
import androidx.media3.common.PlaybackException
import androidx.media3.common.Player
import androidx.media3.exoplayer.ExoPlayer
import androidx.media3.ui.PlayerView
import coil.compose.AsyncImage
import coil.compose.SubcomposeAsyncImage
import coil.request.ImageRequest
import java.io.File
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.collectLatest
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.launch

private val Keep = Color(0xFF37D67A)
private val Reject = Color(0xFFFF5A3C)
private val Amber = Color(0xFFFFB42E)
private val Panel = Color(0xFF121412)
private val Dim = Color(0xFF7D817B)

@Composable
fun CullApp(
    service: EngineService?,
    usbDiag: String,
    resumeTick: Int,
    epoch: Int,
    importDest: String,
    settings: Settings,
    onSaveSettings: (Settings) -> Unit,
    onAlbumUsed: (String) -> Unit,
) {
    MaterialTheme(colorScheme = darkColorScheme(primary = Amber, surface = Panel)) {
        // epoch changes on engine restart (settings save): rebuild everything
        key(epoch) {
            val engine = service?.engine
            var ready by remember { mutableStateOf(false) }
            var status by remember { mutableStateOf("starting engine…") }
            var logTail by remember { mutableStateOf("") }
            var showSettings by remember { mutableStateOf(false) }

            LaunchedEffect(engine) {
                while (true) {
                    try {
                        val err = service?.startError
                        val e = service?.engine
                        if (err != null) {
                            status = "engine failed: " + err
                        } else if (e != null) {
                            ready = e.ready()
                            status = if (ready) "ready" else e.discoveryStatus()
                            logTail = e.recentLog()
                        }
                    } catch (t: Throwable) {
                        status = "engine: ${t.message}"
                    }
                    if (ready) break
                    delay(700)
                }
            }

            var showLog by remember { mutableStateOf(false) }
            when {
                showLog -> LogScreen(
                    fullLog = { service?.engine?.fullLog() ?: "engine not running" },
                    onClose = { showLog = false },
                )
                showSettings -> SettingsScreen(
                    settings,
                    onSave = { onSaveSettings(it); showSettings = false },
                    onClose = { showSettings = false },
                    onLog = { showLog = true },
                )
                !ready || engine == null ->
                    ConnectScreen(
                        status, usbDiag, logTail,
                        onSettings = { showSettings = true },
                        onLog = { showLog = true },
                    )
                else -> CullScreen(
                    Api(engine.port()), importDest, resumeTick, settings,
                    onSettings = { showSettings = true },
                    onAlbumUsed = onAlbumUsed,
                    onLog = { showLog = true },
                )
            }
        }
    }
}

@Composable
private fun ConnectScreen(
    status: String, usbDiag: String = "", logTail: String = "",
    onSettings: (() -> Unit)? = null, onLog: (() -> Unit)? = null,
) {
    Column(
        // background first so it fills behind the bars; content stays clear
        // of the status bar, gesture areas and camera cutouts
        Modifier.fillMaxSize().background(Color(0xFF0B0C0B)).safeDrawingPadding().padding(16.dp),
        verticalArrangement = Arrangement.Center,
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        CircularProgressIndicator(color = Amber)
        Text(
            status,
            color = Dim,
            style = MaterialTheme.typography.bodyMedium,
            modifier = Modifier.padding(24.dp),
            textAlign = TextAlign.Center,
        )
        Text(
            "set the camera to USB card-reader mode and make sure\n" +
                "the phone is the usb host (usb notification →\n" +
                "“USB controlled by: this device”)",
            color = Dim,
            style = MaterialTheme.typography.bodySmall,
            textAlign = TextAlign.Center,
        )
        if (usbDiag.isNotEmpty()) {
            Text(
                usbDiag,
                color = Amber,
                style = MaterialTheme.typography.bodySmall,
                modifier = Modifier.padding(top = 20.dp),
                textAlign = TextAlign.Center,
            )
        }
        Row(Modifier.padding(top = 20.dp)) {
            if (onSettings != null) {
                Text(
                    "settings", color = Dim,
                    style = MaterialTheme.typography.bodySmall,
                    modifier = Modifier.clickable(onClick = onSettings).padding(8.dp),
                )
            }
            if (onLog != null) {
                Text(
                    "log", color = Dim,
                    style = MaterialTheme.typography.bodySmall,
                    modifier = Modifier.clickable(onClick = onLog).padding(8.dp),
                )
            }
        }
        if (logTail.isNotEmpty()) {
            Text(
                logTail,
                color = Color(0xFF565A54),
                fontFamily = FontFamily.Monospace,
                fontSize = 9.sp,
                lineHeight = 13.sp,
                modifier = Modifier.padding(top = 12.dp).fillMaxWidth()
                    .then(if (onLog != null) Modifier.clickable(onClick = onLog) else Modifier),
            )
        }
    }
}

@Composable
private fun LogScreen(fullLog: () -> String, onClose: () -> Unit) {
    BackHandler { onClose() }
    var text by remember { mutableStateOf(fullLog()) }
    val scroll = rememberScrollState()
    val context = LocalContext.current

    LaunchedEffect(Unit) {
        while (true) {
            val atBottom = scroll.value >= scroll.maxValue - 40
            text = fullLog()
            if (atBottom) {
                // follow the tail unless the user scrolled up to read
                kotlinx.coroutines.yield()
                scroll.scrollTo(scroll.maxValue)
            }
            delay(2000)
        }
    }

    Column(Modifier.fillMaxSize().background(Color(0xFF0B0C0B)).safeDrawingPadding()) {
        Row(
            Modifier.fillMaxWidth().background(Panel).padding(8.dp),
            horizontalArrangement = Arrangement.SpaceBetween,
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Text("‹ back", color = Amber, modifier = Modifier.clickable(onClick = onClose).padding(8.dp))
            Text("engine log", color = Color.White)
            Text(
                "share", color = Amber,
                modifier = Modifier.clickable {
                    val send = android.content.Intent(android.content.Intent.ACTION_SEND)
                        .setType("text/plain")
                        .putExtra(android.content.Intent.EXTRA_SUBJECT, "fuji-cull engine log")
                        .putExtra(android.content.Intent.EXTRA_TEXT, fullLog())
                    context.startActivity(android.content.Intent.createChooser(send, "share log"))
                }.padding(8.dp),
            )
        }
        Text(
            text,
            color = Color(0xFF9BA097),
            fontFamily = FontFamily.Monospace,
            fontSize = 10.sp,
            lineHeight = 14.sp,
            modifier = Modifier.fillMaxSize().verticalScroll(scroll).padding(10.dp),
        )
    }
}

@Composable
private fun SettingsScreen(
    initial: Settings, onSave: (Settings) -> Unit, onClose: () -> Unit,
    onLog: (() -> Unit)? = null,
) {
    BackHandler { onClose() }
    var url by remember { mutableStateOf(initial.url) }
    var apiKey by remember { mutableStateOf(initial.key) }
    var session by remember { mutableStateOf(initial.session) }
    var stack by remember { mutableStateOf(initial.stack) }

    Column(
        Modifier.fillMaxSize().background(Color(0xFF0B0C0B)).safeDrawingPadding()
            .verticalScroll(rememberScrollState()).padding(20.dp),
        verticalArrangement = Arrangement.spacedBy(14.dp),
    ) {
        Text("settings", color = Amber, style = MaterialTheme.typography.titleLarge)
        Text(
            "saving restarts the engine (camera connection survives)",
            color = Dim, style = MaterialTheme.typography.bodySmall,
        )
        OutlinedTextField(
            value = url, onValueChange = { url = it },
            label = { Text("immich server url") },
            placeholder = { Text("https://immich.example.com") },
            singleLine = true, modifier = Modifier.fillMaxWidth(),
        )
        OutlinedTextField(
            value = apiKey, onValueChange = { apiKey = it },
            label = { Text("immich api key") },
            singleLine = true, modifier = Modifier.fillMaxWidth(),
        )
        OutlinedTextField(
            value = session, onValueChange = { session = it },
            label = { Text("session name (empty = default)") },
            singleLine = true, modifier = Modifier.fillMaxWidth(),
        )
        Row(verticalAlignment = Alignment.CenterVertically) {
            Switch(checked = stack, onCheckedChange = { stack = it })
            Text(
                "stack RAF+JPG pairs in Immich after upload",
                color = Color.White, modifier = Modifier.padding(start = 10.dp),
            )
        }
        Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.End) {
            if (onLog != null) {
                TextButton(onClick = onLog) { Text("VIEW LOG", color = Dim) }
            }
            TextButton(onClick = onClose) { Text("CANCEL", color = Dim) }
            Button(onClick = {
                onSave(initial.copy(url = url, key = apiKey, session = session, stack = stack))
            }) { Text("SAVE") }
        }
    }
}

@Composable
private fun CullScreen(
    api: Api, importDest: String, resumeTick: Int, settings: Settings,
    onSettings: () -> Unit, onAlbumUsed: (String) -> Unit, onLog: () -> Unit,
) {
    var shots by remember { mutableStateOf(listOf<Shot>()) }
    val decisions = remember { mutableStateOf(mutableMapOf<String, String>()) }
    var thumbStates by remember { mutableStateOf("") }
    var orient by remember { mutableStateOf("") }
    var immich by remember { mutableStateOf("") }
    var sick by remember { mutableStateOf(false) }
    var viewing by remember { mutableIntStateOf(-1) }
    var importing by remember { mutableStateOf("") }
    var showImport by remember { mutableStateOf(false) }
    val scope = rememberCoroutineScope()

    LaunchedEffect(Unit) {
        while (shots.isEmpty()) {
            try {
                val (s, d) = api.state()
                shots = s
                decisions.value = d
            } catch (_: Throwable) {
            }
            if (shots.isEmpty()) delay(1000)
        }
        while (true) {
            // one failed poll (process freeze while backgrounded, engine
            // restart) must not kill this loop — a dead loop is exactly
            // "thumbnails never load again until app restart"
            try {
                val (t, o, im) = api.thumbStates()
                thumbStates = t; orient = o; immich = im
                val st = api.status()
                sick = st.optBoolean("bulkSick") || st.optBoolean("partSick")
                val imp = st.getJSONObject("import")
                if (imp.optBoolean("running")) {
                    importing = "importing ${imp.optInt("done")}/${imp.optInt("total")}"
                } else if (importing.isNotEmpty() && importing != "import done") {
                    importing = imp.optString("error").ifEmpty { "import done" }
                }
            } catch (_: Throwable) {
            }
            delay(2000)
        }
    }

    if (shots.isEmpty()) {
        ConnectScreen("loading catalog…")
        return
    }

    // hoisted above the viewer branch so grid scroll position survives
    // opening and closing the viewer
    val gridState = rememberLazyGridState()
    var returnTo by remember { mutableIntStateOf(-1) }

    if (viewing >= 0) {
        Viewer(
            api, shots, thumbStates, orient, resumeTick, decisions,
            start = viewing,
            onClose = { page ->
                viewing = -1
                returnTo = page
            },
        )
        return
    }

    Column(Modifier.fillMaxSize().background(Color(0xFF0B0C0B)).safeDrawingPadding()) {
        Row(
            Modifier.fillMaxWidth().background(Panel).padding(10.dp),
            horizontalArrangement = Arrangement.SpaceBetween,
            verticalAlignment = Alignment.CenterVertically,
        ) {
            val kept = decisions.value.count { it.value == "keep" }
            val rejected = decisions.value.count { it.value == "reject" }
            Column(Modifier.clickable(onClick = onLog)) {
                Text("K $kept  X $rejected  · ${shots.size - kept - rejected}", color = Color.White)
                val have = thumbStates.count { it == '1' }
                val exKnown = orient.count { it in '1'..'8' }
                val exTotal = orient.count { it != '-' }
                Text(
                    "th $have/${shots.size} · ex $exKnown/$exTotal" + if (sick) " · CAMERA SICK" else "",
                    color = if (sick) Reject else Dim,
                    style = MaterialTheme.typography.bodySmall,
                )
            }
            if (importing.isNotEmpty()) Text(importing, color = Amber)
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text(
                    "⚙",
                    color = Dim, fontSize = 22.sp,
                    modifier = Modifier.clickable(onClick = onSettings).padding(horizontal = 10.dp),
                )
                Button(onClick = { showImport = true }) { Text("IMPORT") }
            }
        }

        LaunchedEffect(returnTo) {
            // land the grid where the viewer left off
            if (returnTo >= 0) {
                gridState.scrollToItem((returnTo - 4).coerceAtLeast(0))
                returnTo = -1
            }
        }
        LaunchedEffect(shots.isEmpty()) {
            // steer the thumbnail sweep at whatever the user is looking at
            snapshotFlow {
                gridState.firstVisibleItemIndex + gridState.layoutInfo.visibleItemsInfo.size / 2
            }
                .distinctUntilChanged()
                .collectLatest {
                    delay(400) // settle after flings
                    api.thumbHint(it.coerceIn(0, shots.size - 1))
                }
        }
        LazyVerticalGrid(columns = GridCells.Adaptive(110.dp), Modifier.fillMaxSize(), state = gridState) {
            itemsIndexed(shots, key = { _, s -> s.id }) { i, shot ->
                GridCell(
                    api, shot,
                    hasThumb = thumbStates.getOrNull(i) == '1',
                    orientC = orient.getOrNull(i) ?: '0',
                    uploaded = immich.getOrNull(i) == '1',
                    decision = decisions.value[shot.id] ?: "",
                    tick = resumeTick,
                    onClick = { viewing = i },
                )
            }
        }
    }

    if (showImport) {
        ImportDialog(
            initialAlbum = settings.album,
            immichConfigured = settings.url.isNotEmpty() && settings.key.isNotEmpty(),
            onStart = { album ->
                importing = "importing…"
                onAlbumUsed(album)
                scope.launch { runCatching { api.startImport(importDest, album) } }
                showImport = false
            },
            onCancel = { showImport = false },
        )
    }
}

@Composable
private fun ImportDialog(
    initialAlbum: String, immichConfigured: Boolean,
    onStart: (String) -> Unit, onCancel: () -> Unit,
) {
    var album by remember { mutableStateOf(initialAlbum) }
    Dialog(onDismissRequest = onCancel) {
        Column(
            Modifier.background(Panel).padding(20.dp),
            verticalArrangement = Arrangement.spacedBy(12.dp),
        ) {
            Text("import keepers", color = Amber, style = MaterialTheme.typography.titleMedium)
            Text(
                if (immichConfigured) {
                    "copies keepers to phone storage, then uploads to Immich"
                } else {
                    "copies keepers to phone storage\n(configure Immich in settings to upload)"
                },
                color = Dim, style = MaterialTheme.typography.bodySmall,
            )
            if (immichConfigured) {
                OutlinedTextField(
                    value = album, onValueChange = { album = it },
                    label = { Text("immich album (optional)") },
                    singleLine = true, modifier = Modifier.fillMaxWidth(),
                )
            }
            Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.End) {
                TextButton(onClick = onCancel) { Text("CANCEL", color = Dim) }
                Button(onClick = { onStart(album) }) { Text("START") }
            }
        }
    }
}

@Composable
private fun GridCell(
    api: Api, shot: Shot, hasThumb: Boolean, orientC: Char,
    uploaded: Boolean, decision: String, tick: Int, onClick: () -> Unit,
) {
    Box(
        Modifier.padding(1.dp).aspectRatio(1.48f).background(Color(0xFF1D201D)).clickable(onClick = onClick),
    ) {
        if (shot.kind == "video") {
            val ctx = LocalContext.current
            val poster by produceState(initialValue = Posters.cached(ctx, shot), shot.id) {
                if (value == null) value = Posters.load(ctx, api, shot)
            }
            poster?.let {
                AsyncImage(
                    model = it,
                    contentDescription = shot.base,
                    modifier = Modifier.fillMaxSize(),
                    contentScale = ContentScale.Crop,
                )
            }
            Box(Modifier.fillMaxWidth().height(3.dp).background(Amber).align(Alignment.TopStart))
        } else if (hasThumb) {
            AsyncImage(
                model = api.thumbUrl(shot.id, orientC, tick),
                contentDescription = shot.base,
                modifier = Modifier.fillMaxSize(),
                contentScale = ContentScale.Crop,
            )
        }
        if (uploaded) {
            Box(Modifier.padding(4.dp).size(8.dp).background(Keep).align(Alignment.TopEnd))
        }
        if (decision.isNotEmpty()) {
            Box(
                Modifier.fillMaxWidth().height(4.dp)
                    .background(if (decision == "keep") Keep else Reject)
                    .align(Alignment.BottomStart),
            )
        }
    }
}

@Composable
private fun Viewer(
    api: Api, shots: List<Shot>, thumbStates: String, orient: String, tick: Int,
    decisions: androidx.compose.runtime.MutableState<MutableMap<String, String>>,
    start: Int, onClose: (Int) -> Unit,
) {
    val pager = rememberPagerState(initialPage = start) { shots.size }
    val scope = rememberCoroutineScope()
    val film = rememberLazyListState()

    BackHandler { onClose(pager.currentPage) }

    LaunchedEffect(pager.currentPage) {
        // the buffer window and the timeline both follow the swipe
        api.cursor(pager.currentPage)
        film.animateScrollToItem((pager.currentPage - 2).coerceAtLeast(0))
    }

    Column(Modifier.fillMaxSize().background(Color.Black).safeDrawingPadding()) {
        Row(
            Modifier.fillMaxWidth().background(Panel).padding(8.dp),
            horizontalArrangement = Arrangement.SpaceBetween,
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Text(
                "‹ grid", color = Amber,
                modifier = Modifier.clickable { onClose(pager.currentPage) }.padding(8.dp),
            )
            val shot = shots[pager.currentPage]
            Text("${shot.folder}/${shot.base}", color = Color.White)
            Text("${pager.currentPage + 1}/${shots.size}", color = Dim)
        }

        HorizontalPager(state = pager, modifier = Modifier.weight(1f), beyondViewportPageCount = 2) { page ->
            val shot = shots[page]
            if (shot.kind == "video") {
                VideoPlayer(api, shot, active = page == pager.currentPage)
            } else {
                ZoomableImage(api, shot)
            }
        }

        // timeline: tap to jump, amber outline marks the current shot
        LazyRow(state = film, modifier = Modifier.fillMaxWidth().background(Panel)) {
            listItemsIndexed(shots, key = { _, s -> s.id }) { i, shot ->
                val current = i == pager.currentPage
                Box(
                    Modifier.padding(2.dp).width(64.dp).height(44.dp)
                        .background(Color(0xFF1D201D))
                        .then(if (current) Modifier.border(2.dp, Amber) else Modifier)
                        .clickable { scope.launch { pager.scrollToPage(i) } },
                ) {
                    val model: Any? = if (shot.kind == "video") {
                        Posters.cached(LocalContext.current, shot)
                    } else if (thumbStates.getOrNull(i) == '1') {
                        api.thumbUrl(shot.id, orient.getOrNull(i) ?: '0', tick)
                    } else null
                    model?.let {
                        AsyncImage(
                            model = it,
                            contentDescription = shot.base,
                            modifier = Modifier.fillMaxSize(),
                            contentScale = ContentScale.Crop,
                        )
                    }
                    decisions.value[shot.id]?.let { d ->
                        Box(
                            Modifier.fillMaxWidth().height(3.dp)
                                .background(if (d == "keep") Keep else Reject)
                                .align(Alignment.BottomStart),
                        )
                    }
                }
            }
        }

        val shot = shots[pager.currentPage]
        val decision = decisions.value[shot.id] ?: ""
        Row(
            Modifier.fillMaxWidth().background(Panel).padding(10.dp),
            horizontalArrangement = Arrangement.SpaceEvenly,
        ) {
            CullButton("REJECT", Reject, decision == "reject") {
                scope.launch {
                    setDecision(api, decisions, shot.id, if (decision == "reject") "" else "reject")
                    if (decision != "reject" && pager.currentPage < shots.size - 1) {
                        pager.animateScrollToPage(pager.currentPage + 1)
                    }
                }
            }
            CullButton("KEEP", Keep, decision == "keep") {
                scope.launch {
                    setDecision(api, decisions, shot.id, if (decision == "keep") "" else "keep")
                    if (decision != "keep" && pager.currentPage < shots.size - 1) {
                        pager.animateScrollToPage(pager.currentPage + 1)
                    }
                }
            }
        }
    }
}

/**
 * Full-screen photo with pinch-zoom, pan and double-tap. At 1x, touches
 * pass through to the pager (swipe navigation); once pinching starts or the
 * image is zoomed, gestures are consumed here. Browsing loads a 4096px
 * decode; zooming in overlays the full-resolution image for 100% sharpness
 * checks (only for the page being zoomed — 26 MP bitmaps are ~100 MB each).
 */
@Composable
private fun ZoomableImage(api: Api, shot: Shot) {
    var retry by remember(shot.id) { mutableIntStateOf(0) }
    var scale by remember(shot.id) { mutableFloatStateOf(1f) }
    var offset by remember(shot.id) { mutableStateOf(Offset.Zero) }
    var box by remember { mutableStateOf(IntSize.Zero) }
    val scope = rememberCoroutineScope()

    fun clamp() {
        val maxX = box.width * (scale - 1f) / 2f
        val maxY = box.height * (scale - 1f) / 2f
        offset = Offset(offset.x.coerceIn(-maxX, maxX), offset.y.coerceIn(-maxY, maxY))
    }

    Box(
        Modifier.fillMaxSize()
            .onSizeChanged { box = it }
            .pointerInput(shot.id) {
                awaitEachGesture {
                    var pinching = false
                    awaitFirstDown(requireUnconsumed = false)
                    while (true) {
                        val event = awaitPointerEvent()
                        if (event.changes.none { it.pressed }) break
                        if (event.changes.size > 1) pinching = true
                        if (!pinching && scale <= 1.01f) continue // pager owns 1x swipes
                        val zoom = event.calculateZoom()
                        val pan = event.calculatePan()
                        val centroid = event.calculateCentroid()
                        val newScale = (scale * zoom).coerceIn(1f, 8f)
                        if (centroid.isSpecified && box != IntSize.Zero) {
                            val center = Offset(box.width / 2f, box.height / 2f)
                            val rel = centroid - center
                            offset = (offset + pan - rel) * (newScale / scale) + rel
                        } else {
                            offset += pan
                        }
                        scale = newScale
                        clamp()
                        event.changes.forEach { it.consume() }
                    }
                    if (scale <= 1.01f) {
                        scale = 1f
                        offset = Offset.Zero
                    }
                }
            }
            .pointerInput("tap-" + shot.id) {
                detectTapGestures(onDoubleTap = { tap ->
                    if (scale > 1.01f) {
                        scale = 1f
                        offset = Offset.Zero
                    } else if (box != IntSize.Zero) {
                        scale = 3f
                        val center = Offset(box.width / 2f, box.height / 2f)
                        offset = (center - tap) * scale
                        clamp()
                    }
                })
            },
    ) {
        Box(
            Modifier.fillMaxSize().graphicsLayer {
                scaleX = scale
                scaleY = scale
                translationX = offset.x
                translationY = offset.y
            },
        ) {
            SubcomposeAsyncImage(
                model = ImageRequest.Builder(LocalContext.current)
                    .data(api.imageUrl(shot.id) + if (retry > 0) "&r=$retry" else "")
                    .size(4096)
                    .build(),
                loading = {
                    Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) {
                        CircularProgressIndicator(color = Amber)
                    }
                },
                error = {
                    Box(
                        Modifier.fillMaxSize().clickable {
                            retry++
                            scope.launch { api.retryShot(shot.id) }
                        },
                        contentAlignment = Alignment.Center,
                    ) {
                        Text("load failed — tap to retry", color = Reject)
                    }
                },
                contentDescription = shot.base,
                modifier = Modifier.fillMaxSize(),
                contentScale = ContentScale.Fit,
            )
            if (scale > 1.3f) {
                // full-res overlay: invisible until decoded, then pixel-sharp
                AsyncImage(
                    model = ImageRequest.Builder(LocalContext.current)
                        .data(api.imageUrl(shot.id))
                        .size(coil.size.Size.ORIGINAL)
                        .build(),
                    contentDescription = null,
                    modifier = Modifier.fillMaxSize(),
                    contentScale = ContentScale.Fit,
                )
            }
        }
    }
}

private suspend fun setDecision(
    api: Api,
    decisions: androidx.compose.runtime.MutableState<MutableMap<String, String>>,
    id: String, value: String,
) {
    val next = decisions.value.toMutableMap()
    if (value.isEmpty()) next.remove(id) else next[id] = value
    decisions.value = next
    runCatching { api.decide(id, value) }
}

@Composable
private fun CullButton(label: String, color: Color, active: Boolean, onClick: () -> Unit) {
    Text(
        label,
        color = if (active) Color.Black else color,
        modifier = Modifier
            .background(if (active) color else Color.Transparent)
            .clickable(onClick = onClick)
            .padding(horizontal = 28.dp, vertical = 12.dp),
    )
}

@Composable
private fun VideoPlayer(api: Api, shot: Shot, active: Boolean) {
    val context = LocalContext.current
    if (!active) {
        // a neighbor page building a player would open a competing camera
        // stream and steal the session from the video actually playing —
        // show the cached poster instead until this page is current
        Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) {
            val poster = Posters.cached(context, shot)
            if (poster != null) {
                AsyncImage(
                    model = poster,
                    contentDescription = shot.base,
                    modifier = Modifier.fillMaxSize(),
                    contentScale = ContentScale.Fit,
                )
            } else {
                Text("video", color = Dim)
            }
        }
        return
    }
    val scope = rememberCoroutineScope()
    val player = remember {
        ExoPlayer.Builder(context).build().apply {
            addListener(object : Player.Listener {
                override fun onPlayerError(error: PlaybackException) {
                    scope.launch {
                        api.logEvent("video ${shot.base}: ${error.errorCodeName} ${error.message}")
                    }
                }
            })
            setMediaItem(MediaItem.fromUri(api.videoUrl(shot.id)))
            prepare()
            playWhenReady = true
        }
    }
    androidx.compose.runtime.DisposableEffect(Unit) {
        onDispose { player.release() }
    }
    AndroidView(
        factory = { PlayerView(it).apply { this.player = player } },
        modifier = Modifier.fillMaxSize(),
    )
}
