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
import androidx.compose.animation.core.animateFloatAsState
import androidx.compose.foundation.gestures.detectTransformGestures
import androidx.compose.foundation.gestures.detectVerticalDragGestures
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.offset
import androidx.compose.foundation.lazy.grid.GridCells
import androidx.compose.foundation.lazy.grid.GridItemSpan
import androidx.compose.foundation.lazy.grid.LazyVerticalGrid
import androidx.compose.foundation.lazy.grid.items
import androidx.compose.foundation.lazy.grid.rememberLazyGridState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.unit.IntOffset
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
import coil.compose.AsyncImage
import coil.compose.SubcomposeAsyncImage
import coil.request.ImageRequest
import java.io.File
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.collectLatest
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

internal val Keep = Color(0xFF37D67A)
internal val Reject = Color(0xFFFF5A3C)
internal val Amber = Color(0xFFFFB42E)
internal val Panel = Color(0xFF121412)
internal val Dim = Color(0xFF7D817B)

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
    onRescan: () -> Unit,
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
                            // gomobile calls off the main thread
                            val snap = withContext(Dispatchers.Default) {
                                val r = e.ready()
                                Triple(r, if (r) "ready" else e.discoveryStatus(), e.recentLog())
                            }
                            ready = snap.first
                            status = snap.second
                            logTail = snap.third
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
                    onRescan = { onRescan(); showSettings = false },
                )
                !ready || engine == null ->
                    ConnectScreen(
                        status, usbDiag, logTail,
                        onSettings = { showSettings = true },
                        onLog = { showLog = true },
                    )
                else -> {
                    // a fresh Api per recomposition would defeat Compose
                    // skipping for every composable that receives it
                    val api = remember(engine) { Api(engine.port()) }
                    CullScreen(
                        api, importDest, resumeTick, settings,
                        onSettings = { showSettings = true },
                        onAlbumUsed = onAlbumUsed,
                        onLog = { showLog = true },
                    )
                }
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
            "set the camera to USB card-reader mode, plug it in, then in the\n" +
                "USB notification set “USB controlled by: connected device”",
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
            text = withContext(Dispatchers.Default) { fullLog() }
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
                    // share the persisted file(s): complete history even when
                    // a disconnect spams the in-memory ring past its capacity
                    val dir = File(context.filesDir, "data")
                    val logs = listOf(File(dir, "engine.log.old"), File(dir, "engine.log"))
                        .filter { it.exists() && it.length() > 0 }
                    val send = if (logs.isNotEmpty()) {
                        val uris = ArrayList(logs.map {
                            androidx.core.content.FileProvider.getUriForFile(
                                context, "pro.zackpollard.fujicull.files", it,
                            )
                        })
                        android.content.Intent(android.content.Intent.ACTION_SEND_MULTIPLE)
                            .setType("text/plain")
                            .putExtra(android.content.Intent.EXTRA_SUBJECT, "fuji-cull engine log")
                            .putParcelableArrayListExtra(android.content.Intent.EXTRA_STREAM, uris)
                            .addFlags(android.content.Intent.FLAG_GRANT_READ_URI_PERMISSION)
                    } else {
                        android.content.Intent(android.content.Intent.ACTION_SEND)
                            .setType("text/plain")
                            .putExtra(android.content.Intent.EXTRA_TEXT, fullLog())
                    }
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
    onLog: (() -> Unit)? = null, onRescan: (() -> Unit)? = null,
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
        if (onRescan != null) {
            Text(
                "full rescan — re-reads the whole camera index (use after " +
                    "deleting in-camera or swapping cards)",
                color = Dim, style = MaterialTheme.typography.bodySmall,
            )
            TextButton(onClick = onRescan) { Text("FULL RESCAN", color = Reject) }
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
    var enginePosters by remember { mutableStateOf(true) } // assume until told otherwise
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
                enginePosters = st.optBoolean("posters")
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

    // MMR fallback poster sweep — only when the engine can't make posters
    // itself (bundled ffmpeg missing). With ffmpeg, the engine's own idle
    // sweep produces video thumbnails exactly like desktop.
    val appCtx = LocalContext.current.applicationContext
    LaunchedEffect("posters") {
        delay(10_000) // let the first /api/status set enginePosters
        if (enginePosters) return@LaunchedEffect
        while (true) {
            var pending = 0
            for (shot in shots) {
                if (shot.kind != "video" || Posters.resolved(appCtx, shot)) continue
                pending++
                Posters.load(appCtx, api, shot)
                delay(400) // yield the link between videos
            }
            if (pending == 0) break
            delay(30_000)
        }
    }

    // hoisted above the viewer branch so grid scroll position survives
    // opening and closing the viewer
    val gridState = rememberLazyGridState()
    var returnTo by remember { mutableIntStateOf(-1) }

    // immich-style timeline rows: date headers between day groups. Shots
    // keep their original indices (thumbStates/orient/immich index by shot).
    val rows = remember(shots) {
        buildList {
            var curMonth = "\u0000"
            var curDay = "\u0000"
            shots.forEachIndexed { i, s ->
                val date = s.date
                val day = date.ifEmpty { s.folder }
                val month = if (date.length >= 7) date.substring(0, 7) else day
                if (month != curMonth) {
                    curMonth = month
                    curDay = "\u0000"
                    add(TimelineRow.MonthHeader(month))
                }
                if (day != curDay) {
                    curDay = day
                    add(TimelineRow.DayHeader(day))
                }
                add(TimelineRow.Cell(s, i))
            }
        }
    }
    val rowOfShot = remember(rows) {
        IntArray(shots.size).also { arr ->
            rows.forEachIndexed { r, row -> if (row is TimelineRow.Cell) arr[row.index] = r }
        }
    }
    val shotAtRow = remember(rows) {
        IntArray(rows.size).also { arr ->
            var last = 0
            rows.forEachIndexed { r, row ->
                if (row is TimelineRow.Cell) last = row.index
                arr[r] = last
            }
        }
    }

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
            val decMap = decisions.value
            val kept = remember(decMap) { decMap.count { it.value == "keep" } }
            val rejected = remember(decMap) { decMap.count { it.value == "reject" } }
            Column(Modifier.clickable(onClick = onLog)) {
                Text("K $kept  X $rejected  · ${shots.size - kept - rejected}", color = Color.White)
                val have = remember(thumbStates) { thumbStates.count { it == '1' } }
                val exKnown = remember(orient) { orient.count { it in '1'..'8' } }
                val exTotal = remember(orient) { orient.count { it != '-' } }
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
                gridState.scrollToItem((rowOfShot.getOrElse(returnTo) { 0 } - 4).coerceAtLeast(0))
                returnTo = -1
            }
        }
        LaunchedEffect(shots.isEmpty()) {
            // steer the thumbnail sweep at whatever the user is looking at
            snapshotFlow {
                gridState.firstVisibleItemIndex + gridState.layoutInfo.visibleItemsInfo.size / 2
            }
                .distinctUntilChanged()
                .collectLatest { row ->
                    delay(400) // settle after flings
                    api.thumbHint(shotAtRow.getOrElse(row.coerceIn(0, rows.lastIndex)) { 0 })
                }
        }
        // scroll-ahead preloading: warm Coil with the tiles about to enter
        // the viewport (byte-identical requests share cache keys with the
        // cells), so flings land on ready bitmaps instead of blanks
        val preloadCtx = LocalContext.current
        LaunchedEffect(shots.isEmpty()) {
            if (shots.isEmpty()) return@LaunchedEffect
            val loader = coil.Coil.imageLoader(preloadCtx)
            val warmed = HashSet<Int>()
            snapshotFlow {
                val first = gridState.firstVisibleItemIndex
                val last = gridState.layoutInfo.visibleItemsInfo.lastOrNull()?.index ?: first
                first to last
            }
                .distinctUntilChanged()
                .collect { (firstRow, lastRow) ->
                    if (warmed.size > 4000) warmed.clear()
                    val first = shotAtRow.getOrElse(firstRow.coerceIn(0, rows.lastIndex)) { 0 }
                    val last = shotAtRow.getOrElse(lastRow.coerceIn(0, rows.lastIndex)) { 0 }
                    val ahead = (last + 1)..minOf(last + 90, shots.lastIndex)
                    val behind = maxOf(first - 45, 0) until first
                    for (i in behind + ahead) {
                        if (thumbStates.getOrNull(i) != '1' || !warmed.add(i)) continue
                        val shot = shots[i]
                        loader.enqueue(
                            thumbRequest(preloadCtx, api, shot.id, orient.getOrNull(i) ?: '0'),
                        )
                    }
                }
        }
        // pinch to change images-per-row (persisted); Immich-style
        val ctxCols = LocalContext.current
        var cols by remember {
            mutableIntStateOf(
                ctxCols.getSharedPreferences("ui", android.content.Context.MODE_PRIVATE)
                    .getInt("gridCols", 3).coerceIn(2, 7),
            )
        }
        var pinchAccum by remember { mutableFloatStateOf(1f) }
        Box(
            Modifier.fillMaxSize().pointerInput(Unit) {
                detectTransformGestures { _, _, zoom, _ ->
                    pinchAccum *= zoom
                    // hysteresis so a single pinch steps one column at a time
                    if (pinchAccum > 1.25f && cols > 2) {
                        cols--; pinchAccum = 1f
                        ctxCols.getSharedPreferences("ui", android.content.Context.MODE_PRIVATE)
                            .edit().putInt("gridCols", cols).apply()
                    } else if (pinchAccum < 0.8f && cols < 7) {
                        cols++; pinchAccum = 1f
                        ctxCols.getSharedPreferences("ui", android.content.Context.MODE_PRIVATE)
                            .edit().putInt("gridCols", cols).apply()
                    }
                }
            },
        ) {
            LazyVerticalGrid(columns = GridCells.Fixed(cols), Modifier.fillMaxSize(), state = gridState) {
                items(
                    count = rows.size,
                    key = { r ->
                        when (val row = rows[r]) {
                            is TimelineRow.MonthHeader -> "m:" + row.month
                            is TimelineRow.DayHeader -> "d:" + row.day
                            is TimelineRow.Cell -> row.shot.id
                        }
                    },
                    contentType = { r ->
                        when (rows[r]) {
                            is TimelineRow.MonthHeader -> "month"
                            is TimelineRow.DayHeader -> "day"
                            is TimelineRow.Cell -> (rows[r] as TimelineRow.Cell).shot.kind
                        }
                    },
                    span = { r ->
                        if (rows[r] is TimelineRow.Cell) GridItemSpan(1) else GridItemSpan(maxLineSpan)
                    },
                ) { r ->
                    when (val row = rows[r]) {
                        is TimelineRow.MonthHeader -> MonthHeaderView(row.month)
                        is TimelineRow.DayHeader -> DayHeaderView(row.day)
                        is TimelineRow.Cell -> {
                            val i = row.index
                            val shot = row.shot
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
            }
            TimelineScrubber(
                gridState = gridState,
                rows = rows,
                modifier = Modifier.align(Alignment.CenterEnd),
            )
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
            if (hasThumb) {
                // engine-made poster (bundled ffmpeg) served like any thumb
                AsyncImage(
                    model = thumbRequest(LocalContext.current, api, shot.id, orientC),
                    contentDescription = shot.base,
                    modifier = Modifier.fillMaxSize(),
                    contentScale = ContentScale.Crop,
                )
            } else {
                val ctx = LocalContext.current
                // memory first (composition-safe); disk stats on IO only
                val poster by produceState(initialValue = Posters.fromMemory(shot), shot.id) {
                    while (value == null) {
                        val (p, dead) = withContext(Dispatchers.IO) {
                            Posters.cached(ctx, shot) to Posters.failed(ctx, shot)
                        }
                        value = p
                        if (p != null || dead) break
                        delay(4000) // the poster sweep may fill it in
                    }
                }
                poster?.let {
                    AsyncImage(
                        model = it,
                        contentDescription = shot.base,
                        modifier = Modifier.fillMaxSize(),
                        contentScale = ContentScale.Crop,
                    )
                }
            }
            Box(Modifier.fillMaxWidth().height(3.dp).background(Amber).align(Alignment.TopStart))
        } else if (hasThumb) {
            AsyncImage(
                model = thumbRequest(LocalContext.current, api, shot.id, orientC),
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
    val filmCtx = LocalContext.current
    LaunchedEffect(pager.currentPage) {
        // warm the filmstrip around the current page
        val loader = coil.Coil.imageLoader(filmCtx)
        val page = pager.currentPage
        for (i in maxOf(0, page - 30)..minOf(shots.lastIndex, page + 30)) {
            if (thumbStates.getOrNull(i) != '1') continue
            loader.enqueue(
                thumbRequest(filmCtx, api, shots[i].id, orient.getOrNull(i) ?: '0'),
            )
        }
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

        HorizontalPager(state = pager, modifier = Modifier.weight(1f), beyondViewportPageCount = 1) { page ->
            val shot = shots[page]
            if (shot.kind == "video") {
                VideoPlayer(api, shot, active = page == pager.currentPage)
            } else {
                ZoomableImage(api, shot)
            }
        }

        // timeline: tap to jump, amber outline marks the current shot
        LazyRow(state = film, modifier = Modifier.fillMaxWidth().background(Panel)) {
            listItemsIndexed(shots, key = { _, s -> s.id }, contentType = { _, s -> s.kind }) { i, shot ->
                val current = i == pager.currentPage
                Box(
                    Modifier.padding(2.dp).width(64.dp).height(44.dp)
                        .background(Color(0xFF1D201D))
                        .then(if (current) Modifier.border(2.dp, Amber) else Modifier)
                        .clickable { scope.launch { pager.scrollToPage(i) } },
                ) {
                    val ctx = LocalContext.current
                    val posterFile by produceState(initialValue = Posters.fromMemory(shot), shot.id) {
                        if (value == null) {
                            value = withContext(Dispatchers.IO) { Posters.cached(ctx, shot) }
                        }
                    }
                    val model: Any? = if (thumbStates.getOrNull(i) == '1') {
                        thumbRequest(ctx, api, shot.id, orient.getOrNull(i) ?: '0')
                    } else if (shot.kind == "video") {
                        posterFile
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

// hand the camera back NOW: the engine-side stream otherwise idles 20s
// holding the claim, blocking sweeps and poster fetches after every video
// peek. Fire-and-forget from a dispose callback, so no composable scope.
@OptIn(kotlinx.coroutines.DelicateCoroutinesApi::class)
internal fun releaseCameraStream(api: Api) {
    kotlinx.coroutines.GlobalScope.launch { api.releaseStream() }
}

/** Timeline rows: a month band, a day header, or a shot cell. */
internal sealed class TimelineRow {
    data class MonthHeader(val month: String) : TimelineRow() // "2026-07" or folder
    data class DayHeader(val day: String) : TimelineRow()     // "2026-07-15" or folder
    data class Cell(val shot: Shot, val index: Int) : TimelineRow()
}

private val monthFmt = java.time.format.DateTimeFormatter.ofPattern("MMMM yyyy")
private val monthShortFmt = java.time.format.DateTimeFormatter.ofPattern("MMM yyyy")
private val dayFmt = java.time.format.DateTimeFormatter.ofPattern("EEE, MMM d, yyyy")

private fun monthDate(key: String) = runCatching {
    java.time.YearMonth.parse(key).atDay(1)
}.getOrNull()

private fun prettyMonth(key: String): String =
    monthDate(key)?.format(monthFmt) ?: key

private fun prettyMonthShort(key: String): String =
    monthDate(key)?.format(monthShortFmt) ?: key

private fun prettyDay(day: String): String = runCatching {
    java.time.LocalDate.parse(day).format(dayFmt)
}.getOrDefault(day)

@Composable
private fun MonthHeaderView(month: String) {
    Text(
        prettyMonth(month),
        color = Color.White,
        style = MaterialTheme.typography.headlineSmall,
        modifier = Modifier.fillMaxWidth().padding(start = 12.dp, top = 20.dp, bottom = 4.dp),
    )
}

@Composable
private fun DayHeaderView(day: String) {
    Text(
        prettyDay(day),
        color = Color(0xFFCED2C9),
        style = MaterialTheme.typography.bodyMedium,
        modifier = Modifier.fillMaxWidth().padding(start = 12.dp, top = 10.dp, bottom = 6.dp),
    )
}

private data class ScrubMark(val row: Int, val frac: Float, val short: String)

/**
 * Immich-style scrubber: a right-edge rail of month labels positioned by
 * their place in the timeline, with a draggable handle that shows a month
 * bubble and jumps the grid. Fades in while scrolling or dragging.
 */
@Composable
private fun TimelineScrubber(
    gridState: androidx.compose.foundation.lazy.grid.LazyGridState,
    rows: List<TimelineRow>,
    modifier: Modifier = Modifier,
) {
    if (rows.size < 2) return
    val scope = rememberCoroutineScope()
    var dragging by remember { mutableStateOf(false) }
    var frac by remember { mutableFloatStateOf(0f) }
    var trackH by remember { mutableIntStateOf(0) }

    // one mark per month band, positioned at its fraction down the list
    val marks = remember(rows) {
        rows.mapIndexedNotNull { r, row ->
            (row as? TimelineRow.MonthHeader)?.let {
                ScrubMark(r, r.toFloat() / rows.lastIndex, prettyMonthShort(it.month))
            }
        }
    }
    fun monthAtFrac(f: Float): String {
        val target = (f * rows.lastIndex).toInt()
        return marks.lastOrNull { it.row <= target }?.short ?: marks.firstOrNull()?.short ?: ""
    }

    val visible = dragging || gridState.isScrollInProgress
    val alpha by animateFloatAsState(if (visible) 1f else 0f, label = "scrubAlpha")
    val posFrac = if (dragging) frac
    else gridState.firstVisibleItemIndex.toFloat() / rows.lastIndex.toFloat()

    val density = LocalDensity.current
    val handlePx = with(density) { 44.dp.toPx() }
    val labelStepPx = with(density) { 22.dp.toPx() }

    Box(
        modifier
            .fillMaxHeight()
            .width(72.dp)
            .onSizeChanged { trackH = it.height }
            .graphicsLayer { this.alpha = alpha }
            .pointerInput(rows.size) {
                detectVerticalDragGestures(
                    onDragStart = { off ->
                        dragging = true
                        frac = (off.y / size.height).coerceIn(0f, 1f)
                        val t = (frac * rows.lastIndex).toInt().coerceIn(0, rows.lastIndex)
                        scope.launch { gridState.scrollToItem(t) }
                    },
                    onDragEnd = { dragging = false },
                    onDragCancel = { dragging = false },
                ) { change, _ ->
                    frac = (change.position.y / size.height).coerceIn(0f, 1f)
                    val t = (frac * rows.lastIndex).toInt().coerceIn(0, rows.lastIndex)
                    scope.launch { gridState.scrollToItem(t) }
                    change.consume()
                }
            },
    ) {
        // month labels down the rail, thinned so they never overlap
        var lastY = -1e9f
        for (m in marks) {
            val y = m.frac * (trackH - handlePx)
            if (y - lastY < labelStepPx) continue
            lastY = y
            Text(
                m.short,
                color = Color(0xFFB7BBB2),
                fontSize = 11.sp,
                modifier = Modifier
                    .align(Alignment.TopEnd)
                    .offset { IntOffset(0, y.toInt()) }
                    .padding(end = 10.dp),
            )
        }
        // handle
        val yOff = (posFrac * (trackH - handlePx)).toInt().coerceAtLeast(0)
        Box(
            Modifier
                .align(Alignment.TopEnd)
                .offset { IntOffset(0, yOff) }
                .padding(end = 4.dp)
                .size(44.dp)
                .background(Color(0xFF3A3D39), androidx.compose.foundation.shape.CircleShape),
            contentAlignment = Alignment.Center,
        ) {
            Text("⇅", color = Color.White, fontSize = 18.sp)
        }
        // month bubble while dragging
        if (dragging) {
            val label = monthAtFrac(frac)
            if (label.isNotEmpty()) {
                Text(
                    label,
                    color = Color.White,
                    fontSize = 14.sp,
                    modifier = Modifier
                        .align(Alignment.TopEnd)
                        .offset { IntOffset(-52.dp.roundToPx(), (yOff + 4).coerceAtLeast(0)) }
                        .background(Color(0xF2222522), RoundedCornerShape(14.dp))
                        .padding(horizontal = 14.dp, vertical = 8.dp),
                )
            }
        }
    }
}

// thumbRequest builds the ONE canonical request shape for a shot's
// thumbnail — cells, filmstrip and preloaders must all use it so Coil's
// cache keys line up and a preloaded tile composes with zero work
private fun thumbRequest(
    ctx: android.content.Context, api: Api, id: String, orientC: Char,
): ImageRequest =
    ImageRequest.Builder(ctx)
        // stable URL: Coil's disk cache then persists thumbs across app
        // launches (the old &rt= buster forced cold reloads every open)
        .data(api.thumbUrl(id, orientC))
        .size(256)
        .build()

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
            val poster by produceState(initialValue = Posters.fromMemory(shot), shot.id) {
                if (value == null) {
                    value = withContext(Dispatchers.IO) { Posters.cached(context, shot) }
                }
            }
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
    MpvPlayer(api, shot, Modifier.fillMaxSize())
}
