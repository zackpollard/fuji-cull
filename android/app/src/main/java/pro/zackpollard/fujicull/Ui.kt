package pro.zackpollard.fujicull

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.aspectRatio
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.lazy.grid.GridCells
import androidx.compose.foundation.lazy.grid.LazyVerticalGrid
import androidx.compose.foundation.lazy.grid.itemsIndexed
import androidx.compose.foundation.pager.HorizontalPager
import androidx.compose.foundation.pager.rememberPagerState
import androidx.compose.material3.Button
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.darkColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.ui.viewinterop.AndroidView
import androidx.media3.common.MediaItem
import androidx.media3.exoplayer.ExoPlayer
import androidx.media3.ui.PlayerView
import coil.compose.AsyncImage
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch

private val Keep = Color(0xFF37D67A)
private val Reject = Color(0xFFFF5A3C)
private val Amber = Color(0xFFFFB42E)
private val Panel = Color(0xFF121412)

@Composable
fun CullApp(service: EngineService?, usbDiag: String, importDest: String) {
    MaterialTheme(colorScheme = darkColorScheme(primary = Amber, surface = Panel)) {
        val engine = service?.engine
        var ready by remember { mutableStateOf(false) }
        var status by remember { mutableStateOf("starting engine…") }
        var logTail by remember { mutableStateOf("") }

        LaunchedEffect(engine) {
            while (true) {
                val err = service?.startError
                val e = service?.engine
                if (err != null) {
                    status = "engine failed: " + err
                } else if (e != null) {
                    ready = e.ready()
                    status = if (ready) "ready" else e.discoveryStatus()
                    logTail = e.recentLog()
                }
                if (ready) break
                delay(700)
            }
        }

        if (!ready || engine == null) {
            ConnectScreen(status, usbDiag, logTail)
        } else {
            CullScreen(Api(engine.port()), importDest)
        }
    }
}

@Composable
private fun ConnectScreen(status: String, usbDiag: String = "", logTail: String = "") {
    Column(
        Modifier.fillMaxSize().background(Color(0xFF0B0C0B)).padding(16.dp),
        verticalArrangement = Arrangement.Center,
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        CircularProgressIndicator(color = Amber)
        Text(
            status,
            color = Color(0xFF7D817B),
            style = MaterialTheme.typography.bodyMedium,
            modifier = Modifier.padding(24.dp),
            textAlign = TextAlign.Center,
        )
        Text(
            "set the camera to USB card-reader mode and make sure\n" +
                "the phone is the usb host (usb notification →\n" +
                "“USB controlled by: this device”)",
            color = Color(0xFF7D817B),
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
        if (logTail.isNotEmpty()) {
            Text(
                logTail,
                color = Color(0xFF565A54),
                fontFamily = FontFamily.Monospace,
                fontSize = 9.sp,
                lineHeight = 13.sp,
                modifier = Modifier.padding(top = 20.dp).fillMaxWidth(),
            )
        }
    }
}

@Composable
private fun CullScreen(api: Api, importDest: String) {
    var shots by remember { mutableStateOf(listOf<Shot>()) }
    val decisions = remember { mutableStateOf(mutableMapOf<String, String>()) }
    var thumbStates by remember { mutableStateOf("") }
    var orient by remember { mutableStateOf("") }
    var immich by remember { mutableStateOf("") }
    var viewing by remember { mutableIntStateOf(-1) }
    var importing by remember { mutableStateOf("") }
    val scope = rememberCoroutineScope()

    LaunchedEffect(Unit) {
        val (s, d) = api.state()
        shots = s
        decisions.value = d
        while (true) {
            val (t, o, im) = api.thumbStates()
            thumbStates = t; orient = o; immich = im
            if (importing.isNotEmpty()) {
                val st = api.importStatus()
                importing = if (st.optBoolean("running")) {
                    "importing ${st.optInt("done")}/${st.optInt("total")}"
                } else st.optString("error").ifEmpty { "import done" }
            }
            delay(2000)
        }
    }

    if (shots.isEmpty()) {
        ConnectScreen("loading catalog…")
        return
    }

    if (viewing >= 0) {
        Viewer(api, shots, decisions, start = viewing, onClose = { viewing = -1 })
        return
    }

    Column(Modifier.fillMaxSize().background(Color(0xFF0B0C0B))) {
        Row(
            Modifier.fillMaxWidth().background(Panel).padding(10.dp),
            horizontalArrangement = Arrangement.SpaceBetween,
            verticalAlignment = Alignment.CenterVertically,
        ) {
            val kept = decisions.value.count { it.value == "keep" }
            val rejected = decisions.value.count { it.value == "reject" }
            Text("K $kept  X $rejected  · ${shots.size - kept - rejected}", color = Color.White)
            if (importing.isNotEmpty()) Text(importing, color = Amber)
            Button(onClick = {
                importing = "importing…"
                scope.launch { api.startImport(importDest, "") }
            }) { Text("IMPORT") }
        }
        LazyVerticalGrid(columns = GridCells.Adaptive(110.dp), Modifier.fillMaxSize()) {
            itemsIndexed(shots, key = { _, s -> s.id }) { i, shot ->
                GridCell(
                    api, shot,
                    hasThumb = thumbStates.getOrNull(i) == '1',
                    orientC = orient.getOrNull(i) ?: '0',
                    uploaded = immich.getOrNull(i) == '1',
                    decision = decisions.value[shot.id] ?: "",
                    onClick = { viewing = i },
                )
            }
        }
    }
}

@Composable
private fun GridCell(
    api: Api, shot: Shot, hasThumb: Boolean, orientC: Char,
    uploaded: Boolean, decision: String, onClick: () -> Unit,
) {
    Box(
        Modifier.padding(1.dp).aspectRatio(1.48f).background(Color(0xFF1D201D)).clickable(onClick = onClick),
    ) {
        if (hasThumb) {
            AsyncImage(
                model = api.thumbUrl(shot.id, orientC),
                contentDescription = shot.base,
                modifier = Modifier.fillMaxSize(),
                contentScale = androidx.compose.ui.layout.ContentScale.Crop,
            )
        }
        if (shot.kind == "video") {
            Box(Modifier.fillMaxWidth().height(3.dp).background(Amber).align(Alignment.TopStart))
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
    api: Api, shots: List<Shot>,
    decisions: androidx.compose.runtime.MutableState<MutableMap<String, String>>,
    start: Int, onClose: () -> Unit,
) {
    val pager = rememberPagerState(initialPage = start) { shots.size }
    val scope = rememberCoroutineScope()

    Column(Modifier.fillMaxSize().background(Color.Black)) {
        Row(
            Modifier.fillMaxWidth().background(Panel).padding(8.dp),
            horizontalArrangement = Arrangement.SpaceBetween,
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Text("‹ grid", color = Amber, modifier = Modifier.clickable(onClick = onClose).padding(8.dp))
            val shot = shots[pager.currentPage]
            Text("${shot.folder}/${shot.base}", color = Color.White)
            Text("${pager.currentPage + 1}/${shots.size}", color = Color(0xFF7D817B))
        }

        HorizontalPager(state = pager, modifier = Modifier.weight(1f), beyondViewportPageCount = 2) { page ->
            val shot = shots[page]
            if (shot.kind == "video") {
                VideoPlayer(api.videoUrl(shot.id), active = page == pager.currentPage)
            } else {
                AsyncImage(
                    model = api.imageUrl(shot.id),
                    contentDescription = shot.base,
                    modifier = Modifier.fillMaxSize(),
                    contentScale = androidx.compose.ui.layout.ContentScale.Fit,
                )
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

private suspend fun setDecision(
    api: Api,
    decisions: androidx.compose.runtime.MutableState<MutableMap<String, String>>,
    id: String, value: String,
) {
    val next = decisions.value.toMutableMap()
    if (value.isEmpty()) next.remove(id) else next[id] = value
    decisions.value = next
    api.decide(id, value)
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
private fun VideoPlayer(url: String, active: Boolean) {
    val context = androidx.compose.ui.platform.LocalContext.current
    val player = remember {
        ExoPlayer.Builder(context).build().apply {
            setMediaItem(MediaItem.fromUri(url))
            prepare()
        }
    }
    LaunchedEffect(active) { player.playWhenReady = active }
    androidx.compose.runtime.DisposableEffect(Unit) {
        onDispose { player.release() }
    }
    AndroidView(
        factory = { PlayerView(it).apply { this.player = player } },
        modifier = Modifier.fillMaxSize(),
    )
}
