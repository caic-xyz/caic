// Collapsed past turn: shows summary, expands to full content on click.
package com.fghbuild.caic.ui.taskdetail

import androidx.compose.animation.AnimatedVisibility
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import com.fghbuild.caic.util.Turn
import com.fghbuild.caic.util.turnSummary

@Composable
fun ElidedTurn(turn: Turn) {
    val turnKey = turn.groups.firstOrNull()?.events?.firstOrNull()?.ts?.toString() ?: "0"
    var expanded by rememberSaveable(turnKey) { mutableStateOf(false) }

    Column(modifier = Modifier.fillMaxWidth()) {
        Text(
            text = turnSummary(turn),
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
            modifier = Modifier
                .fillMaxWidth()
                .clickable { expanded = !expanded }
                .padding(vertical = 4.dp),
        )
        AnimatedVisibility(visible = expanded) {
            TurnContent(turn = turn, onAnswer = null)
        }
    }
}
