// Full-width bottom shell panel composable: settings, voice controls, attention, and transcript display.
package com.fghbuild.gomode.voice

import androidx.compose.animation.core.RepeatMode
import androidx.compose.animation.core.animateFloat
import androidx.compose.animation.core.animateFloatAsState
import androidx.compose.animation.core.infiniteRepeatable
import androidx.compose.animation.core.rememberInfiniteTransition
import androidx.compose.animation.core.tween
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.navigationBars
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.windowInsetsPadding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.itemsIndexed
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.CallEnd
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.ExpandLess
import androidx.compose.material.icons.filled.ExpandMore
import androidx.compose.material.icons.filled.Mic
import androidx.compose.material.icons.filled.MicOff
import androidx.compose.material.icons.filled.Settings
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
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.alpha
import androidx.compose.ui.draw.clip
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.text.SpanStyle
import androidx.compose.ui.text.buildAnnotatedString
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.withStyle
import androidx.compose.ui.unit.dp

private const val PulseMinAlpha = 0.5f
private const val PulseMaxAlpha = 1.0f
private const val PulseDurationMs = 1000
private const val BarCount = 3
private const val BarMinHeight = 4f
private const val BarMaxHeight = 20f
private const val BarContainerSize = 24
private val TranscriptHeight = 220.dp

@Composable
fun VoicePanel(
    voiceState: VoiceState,
    voiceEnabled: Boolean,
    onConnect: () -> Unit,
    onDisconnect: () -> Unit,
    onToggleMute: () -> Unit,
    onSelectDevice: (Int) -> Unit,
    onClearTranscript: () -> Unit,
    onOpenSettings: () -> Unit,
    serviceAttentionText: String?,
    modifier: Modifier = Modifier,
) {
    Surface(
        modifier = modifier,
        tonalElevation = 4.dp,
    ) {
        Column(modifier = Modifier.windowInsetsPadding(WindowInsets.navigationBars)) {
            HorizontalDivider(color = MaterialTheme.colorScheme.outline)
            when {
                !voiceEnabled -> IdlePanel(onConnect, onOpenSettings, voiceEnabled = false)
                voiceState.error != null -> ErrorPanel(voiceState.error, onConnect, onOpenSettings)
                voiceState.connectStatus != null -> ConnectingPanel(voiceState.connectStatus, onOpenSettings)
                voiceState.listening || voiceState.speaking -> ActivePanel(
                    voiceState = voiceState,
                    onDisconnect = onDisconnect,
                    onToggleMute = onToggleMute,
                    onSelectDevice = onSelectDevice,
                    onClearTranscript = onClearTranscript,
                    onOpenSettings = onOpenSettings,
                )
                !voiceState.connected -> IdlePanel(onConnect, onOpenSettings)
                else -> ConnectingPanel("Starting audio…", onOpenSettings)
            }
            ServiceAttentionLabel(serviceAttentionText)
        }
    }
}

@Composable
private fun ServiceAttentionLabel(text: String?) {
    if (text == null) return
    Text(
        text = text,
        style = MaterialTheme.typography.labelMedium,
        color = MaterialTheme.colorScheme.error,
        modifier = Modifier
            .fillMaxWidth()
            .padding(start = 16.dp, end = 16.dp, bottom = 8.dp)
            .testTag("gomode-service-attention"),
    )
}

@Composable
private fun SettingsButton(onOpenSettings: () -> Unit) {
    IconButton(
        onClick = onOpenSettings,
        modifier = Modifier.testTag("gomode-web-open-settings"),
    ) {
        Icon(
            Icons.Default.Settings,
            contentDescription = "Settings",
            modifier = Modifier.size(24.dp),
        )
    }
}

@Composable
private fun IdlePanel(
    onConnect: () -> Unit,
    onOpenSettings: () -> Unit,
    voiceEnabled: Boolean = true,
) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .padding(start = 16.dp, top = 0.dp, end = 16.dp, bottom = 8.dp),
        horizontalArrangement = Arrangement.SpaceBetween,
        verticalAlignment = Alignment.CenterVertically,
    ) {
        SettingsButton(onOpenSettings)
        val disabledAlpha = 0.38f
        val iconAlpha = if (voiceEnabled) 1f else disabledAlpha
        IconButton(
            onClick = onConnect,
            enabled = voiceEnabled,
            modifier = Modifier
                .size(36.dp)
                .border(
                    width = 1.dp,
                    color = MaterialTheme.colorScheme.outline.copy(alpha = iconAlpha),
                    shape = CircleShape,
                )
                .testTag(if (voiceEnabled) "gomode-voice-connect" else "gomode-voice-disabled"),
        ) {
            Icon(
                Icons.Default.Mic,
                contentDescription = if (voiceEnabled) "Connect voice assistant" else "Voice unavailable",
                tint = MaterialTheme.colorScheme.onSurfaceVariant.copy(alpha = iconAlpha),
                modifier = Modifier.size(20.dp),
            )
        }
    }
}

@Composable
private fun ConnectingPanel(
    status: String,
    onOpenSettings: () -> Unit,
) {
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
            .padding(start = 16.dp, top = 0.dp, end = 16.dp, bottom = 12.dp)
            .alpha(alpha),
        verticalAlignment = Alignment.CenterVertically,
        horizontalArrangement = Arrangement.spacedBy(12.dp),
    ) {
        SettingsButton(onOpenSettings)
        Icon(Icons.Default.Mic, contentDescription = null)
        Text(
            text = status,
            style = MaterialTheme.typography.bodyMedium,
            modifier = Modifier.weight(1f),
        )
        IconButton(onClick = {}) {
            Icon(Icons.Default.Close, contentDescription = "Cancel")
        }
    }
}

@Composable
private fun ActivePanel(
    voiceState: VoiceState,
    onDisconnect: () -> Unit,
    onToggleMute: () -> Unit,
    onSelectDevice: (Int) -> Unit,
    onClearTranscript: () -> Unit,
    onOpenSettings: () -> Unit,
) {
    var transcriptExpanded by rememberSaveable { mutableStateOf(false) }

    Column(
        modifier = Modifier.padding(start = 16.dp, top = 0.dp, end = 16.dp, bottom = 12.dp),
        verticalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        Row(
            verticalAlignment = Alignment.CenterVertically,
            horizontalArrangement = Arrangement.spacedBy(12.dp),
        ) {
            SettingsButton(onOpenSettings)
            MicLevelIndicator(micLevel = voiceState.micLevel)

            val statusText = when {
                voiceState.activeTool != null -> voiceState.activeTool!!
                voiceState.muted && !voiceState.speaking -> "Muted"
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

            IconButton(onClick = onToggleMute) {
                Icon(
                    imageVector = if (voiceState.muted) Icons.Default.MicOff else Icons.Default.Mic,
                    contentDescription = if (voiceState.muted) "Unmute" else "Mute",
                )
            }

            IconButton(onClick = onDisconnect) {
                Icon(
                    Icons.Default.CallEnd,
                    contentDescription = "End voice",
                    tint = MaterialTheme.colorScheme.error,
                )
            }
        }

        if (voiceState.availableDevices.size > 1) {
            AudioDevicePicker(
                devices = voiceState.availableDevices,
                selectedDeviceId = voiceState.selectedDeviceId,
                onSelect = onSelectDevice,
            )
        }

        if (voiceState.transcript.isNotEmpty()) {
            Row(
                modifier = Modifier.fillMaxWidth(),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Text(
                    text = "Transcript",
                    style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.weight(1f),
                )
                IconButton(onClick = onClearTranscript, modifier = Modifier.size(32.dp)) {
                    Icon(
                        Icons.Default.Close,
                        contentDescription = "Clear transcript",
                        modifier = Modifier.size(16.dp),
                    )
                }
                IconButton(
                    onClick = { transcriptExpanded = !transcriptExpanded },
                    modifier = Modifier
                        .size(32.dp)
                        .testTag("gomode-voice-transcript-toggle"),
                ) {
                    Icon(
                        imageVector = if (transcriptExpanded) Icons.Default.ExpandLess else Icons.Default.ExpandMore,
                        contentDescription = if (transcriptExpanded) "Hide transcript" else "Show transcript",
                        modifier = Modifier.size(20.dp),
                    )
                }
            }
        }
        if (transcriptExpanded) {
            TranscriptLog(
                entries = voiceState.transcript,
                modifier = Modifier
                    .fillMaxWidth()
                    .testTag("gomode-voice-transcript"),
            )
        }
    }
}

@Composable
private fun ErrorPanel(
    message: String,
    onReconnect: () -> Unit,
    onOpenSettings: () -> Unit,
) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .padding(start = 16.dp, top = 0.dp, end = 16.dp, bottom = 12.dp),
        verticalAlignment = Alignment.CenterVertically,
        horizontalArrangement = Arrangement.spacedBy(12.dp),
    ) {
        SettingsButton(onOpenSettings)
        Icon(
            Icons.Default.Mic,
            contentDescription = null,
            tint = MaterialTheme.colorScheme.error,
        )
        Column(modifier = Modifier.weight(1f)) {
            Text(
                text = "Voice connection unavailable",
                style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.error,
            )
            Text(
                text = message,
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
        Button(onClick = onReconnect) {
            Text("Reconnect")
        }
    }
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
private fun TranscriptLog(
    entries: List<TranscriptEntry>,
    modifier: Modifier = Modifier,
) {
    val listState = rememberLazyListState()
    val lastEntryText = entries.lastOrNull()?.text
    LaunchedEffect(entries.size, lastEntryText) {
        if (entries.isNotEmpty()) {
            listState.scrollToItem(entries.size - 1)
        }
    }
    if (entries.isNotEmpty()) {
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
private fun MicLevelIndicator(micLevel: Float = 0f) {
    val durations = intArrayOf(80, 40, 120)
    Box(
        modifier = Modifier.size(BarContainerSize.dp),
        contentAlignment = Alignment.Center,
    ) {
        Row(
            horizontalArrangement = Arrangement.spacedBy(2.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            repeat(BarCount) { index ->
                val target = BarMinHeight + micLevel * (BarMaxHeight - BarMinHeight)
                val height by animateFloatAsState(
                    targetValue = target,
                    animationSpec = tween(durationMillis = durations[index]),
                    label = "bar$index",
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
}

