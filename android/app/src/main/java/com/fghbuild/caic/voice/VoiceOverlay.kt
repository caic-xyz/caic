// Bottom voice panel composable: mic button, status, and transcription display.
package com.fghbuild.caic.voice

import androidx.compose.animation.core.RepeatMode
import androidx.compose.animation.core.animateFloat
import androidx.compose.animation.core.animateFloatAsState
import androidx.compose.animation.core.infiniteRepeatable
import androidx.compose.animation.core.rememberInfiniteTransition
import androidx.compose.animation.core.tween
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.navigationBars
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.windowInsetsPadding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.itemsIndexed
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Mic
import androidx.compose.material.icons.filled.Stop
import androidx.compose.material3.Button
import androidx.compose.material3.FilterChip
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.alpha
import androidx.compose.ui.draw.clip
import androidx.compose.ui.text.SpanStyle
import androidx.compose.ui.text.buildAnnotatedString
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.withStyle
import androidx.compose.ui.unit.dp

private const val PulseMinAlpha = 0.5f
private const val PulseMaxAlpha = 1.0f
private const val PulseDurationMs = 1000
private const val BarAnimDurationMs = 400
private const val BarCount = 3
private const val BarMinHeight = 4
private const val BarMaxHeight = 20
private const val MicLevelMinSize = 10
private const val MicLevelMaxSize = 18
private val TranscriptHeight = 220.dp

@Composable
fun VoicePanel(
    voiceState: VoiceState,
    voiceEnabled: Boolean,
    onConnect: () -> Unit,
    onDisconnect: () -> Unit,
    onSelectDevice: (Int) -> Unit,
    modifier: Modifier = Modifier,
) {
    if (!voiceEnabled) return

    Surface(
        modifier = modifier,
        tonalElevation = 4.dp,
    ) {
        Column(modifier = Modifier.windowInsetsPadding(WindowInsets.navigationBars)) {
            HorizontalDivider()
            when {
                voiceState.error != null -> ErrorPanel(onConnect)
                voiceState.connectStatus != null -> ConnectingPanel(voiceState.connectStatus)
                voiceState.listening || voiceState.speaking -> ActivePanel(
                    voiceState = voiceState,
                    onDisconnect = onDisconnect,
                    onSelectDevice = onSelectDevice,
                )
                !voiceState.connected -> IdlePanel(onConnect)
                else -> ConnectingPanel("Starting audio…")
            }
        }
    }
}

@Composable
private fun IdlePanel(onClick: () -> Unit) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .padding(horizontal = 16.dp, vertical = 12.dp),
        verticalAlignment = Alignment.CenterVertically,
        horizontalArrangement = Arrangement.spacedBy(12.dp),
    ) {
        Icon(
            Icons.Default.Mic,
            contentDescription = null,
            tint = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        Text(
            text = "Voice assistant",
            style = MaterialTheme.typography.bodyMedium,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
            modifier = Modifier.weight(1f),
        )
        Button(onClick = onClick) {
            Text("Connect")
        }
    }
}

@Composable
private fun ConnectingPanel(status: String) {
    val infiniteTransition = rememberInfiniteTransition(label = "pulse")
    val alpha by infiniteTransition.animateFloat(
        initialValue = PulseMinAlpha,
        targetValue = PulseMaxAlpha,
        animationSpec = infiniteRepeatable(
            animation = tween(durationMillis = PulseDurationMs),
            repeatMode = RepeatMode.Reverse,
        ),
        label = "pulseAlpha",
    )
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .alpha(alpha)
            .padding(horizontal = 16.dp, vertical = 12.dp),
        verticalAlignment = Alignment.CenterVertically,
        horizontalArrangement = Arrangement.spacedBy(12.dp),
    ) {
        Icon(Icons.Default.Mic, contentDescription = null)
        Text(text = status, style = MaterialTheme.typography.bodyMedium)
    }
}

@Composable
private fun ActivePanel(
    voiceState: VoiceState,
    onDisconnect: () -> Unit,
    onSelectDevice: (Int) -> Unit,
) {
    Column(
        modifier = Modifier
            .fillMaxWidth()
            .padding(horizontal = 16.dp, vertical = 12.dp),
        verticalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        Row(
            verticalAlignment = Alignment.CenterVertically,
            horizontalArrangement = Arrangement.spacedBy(12.dp),
        ) {
            if (voiceState.speaking) {
                SpeakingIndicator()
            } else {
                ListeningIndicator(micLevel = voiceState.micLevel)
            }

            val statusText = when {
                voiceState.activeTool != null -> voiceState.activeTool!!
                voiceState.speaking -> "Speaking…"
                else -> "Listening…"
            }
            Text(
                text = statusText,
                style = MaterialTheme.typography.bodyMedium,
                color = if (voiceState.activeTool != null) {
                    MaterialTheme.colorScheme.tertiary
                } else {
                    MaterialTheme.colorScheme.onSurface
                },
                modifier = Modifier.weight(1f),
            )

            IconButton(onClick = onDisconnect) {
                Icon(Icons.Default.Stop, contentDescription = "End voice")
            }
        }

        if (voiceState.availableDevices.size > 1) {
            AudioDevicePicker(
                devices = voiceState.availableDevices,
                selectedDeviceId = voiceState.selectedDeviceId,
                onSelect = onSelectDevice,
            )
        }

        TranscriptLog(
            entries = voiceState.transcript,
            modifier = Modifier.fillMaxWidth(),
        )
    }
}

@Composable
private fun ErrorPanel(onClick: () -> Unit) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .padding(horizontal = 16.dp, vertical = 12.dp),
        verticalAlignment = Alignment.CenterVertically,
        horizontalArrangement = Arrangement.spacedBy(12.dp),
    ) {
        Icon(
            Icons.Default.Mic,
            contentDescription = null,
            tint = MaterialTheme.colorScheme.error,
        )
        Text(
            text = "Voice error — tap to retry",
            style = MaterialTheme.typography.bodyMedium,
            color = MaterialTheme.colorScheme.error,
            modifier = Modifier.weight(1f),
        )
        Button(onClick = onClick) {
            Text("Retry")
        }
    }
}

@Composable
private fun TranscriptLog(
    entries: List<TranscriptEntry>,
    modifier: Modifier = Modifier,
) {
    val listState = rememberLazyListState()
    // Scroll to bottom whenever the last entry changes (new entry or partial update).
    val lastEntryText = entries.lastOrNull()?.text
    LaunchedEffect(entries.size, lastEntryText) {
        if (entries.isNotEmpty()) {
            listState.scrollToItem(entries.size - 1)
        }
    }
    if (entries.isEmpty()) {
        Text(
            text = "Transcript will appear here…",
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant.copy(alpha = 0.5f),
            modifier = modifier.padding(vertical = 8.dp),
        )
    } else {
        LazyColumn(
            state = listState,
            modifier = modifier.height(TranscriptHeight),
            verticalArrangement = Arrangement.spacedBy(4.dp),
        ) {
            itemsIndexed(entries) { _, entry ->
                val isUser = entry.speaker == TranscriptSpeaker.USER
                val label = if (isUser) "You" else "Assistant"
                val labelColor = if (isUser) {
                    MaterialTheme.colorScheme.primary
                } else {
                    MaterialTheme.colorScheme.secondary
                }
                Text(
                    text = buildAnnotatedString {
                        withStyle(SpanStyle(color = labelColor, fontWeight = FontWeight.SemiBold)) {
                            append("$label: ")
                        }
                        append(entry.text)
                    },
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurface,
                    modifier = Modifier.fillMaxWidth(),
                )
            }
        }
    }
}

@Composable
private fun ListeningIndicator(micLevel: Float = 0f) {
    val sizeDp = MicLevelMinSize + micLevel * (MicLevelMaxSize - MicLevelMinSize)
    val size by animateFloatAsState(targetValue = sizeDp, label = "micSize")
    Box(
        modifier = Modifier
            .size(size.dp)
            .clip(CircleShape)
            .background(MaterialTheme.colorScheme.primary),
    )
}

@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun AudioDevicePicker(
    devices: List<AudioDevice>,
    selectedDeviceId: Int?,
    onSelect: (Int) -> Unit,
) {
    FlowRow(
        horizontalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        devices.forEach { device ->
            FilterChip(
                selected = device.id == selectedDeviceId,
                onClick = { onSelect(device.id) },
                label = { Text(device.name, style = MaterialTheme.typography.labelSmall) },
            )
        }
    }
}

@Composable
private fun SpeakingIndicator() {
    val infiniteTransition = rememberInfiniteTransition(label = "speaking")
    Row(
        horizontalArrangement = Arrangement.spacedBy(2.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        repeat(BarCount) { index ->
            val fraction by infiniteTransition.animateFloat(
                initialValue = 0f,
                targetValue = 1f,
                animationSpec = infiniteRepeatable(
                    animation = tween(
                        durationMillis = BarAnimDurationMs,
                        delayMillis = index * (BarAnimDurationMs / BarCount),
                    ),
                    repeatMode = RepeatMode.Reverse,
                ),
                label = "bar$index",
            )
            val heightTarget = BarMinHeight + fraction * (BarMaxHeight - BarMinHeight)
            val height by animateFloatAsState(
                targetValue = heightTarget,
                label = "barHeight$index",
            )
            Box(
                modifier = Modifier
                    .width(3.dp)
                    .height(height.dp)
                    .clip(RoundedCornerShape(1.dp))
                    .background(MaterialTheme.colorScheme.primary),
            )
        }
    }
}
