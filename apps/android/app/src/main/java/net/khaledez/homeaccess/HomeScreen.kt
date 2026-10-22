package net.khaledez.homeaccess

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.material3.Button
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle

@Composable
fun HomeScreen(vm: GuestViewModel) {
    val busy by vm.isBusy.collectAsStateWithLifecycle()
    val status by vm.actionStatus.collectAsStateWithLifecycle()

    Column(
        Modifier.fillMaxSize().padding(24.dp),
        verticalArrangement = Arrangement.spacedBy(16.dp, Alignment.CenterVertically),
        horizontalAlignment = Alignment.CenterHorizontally,
    ) {
        DoorButton("Open Gate", busy) { vm.openGate() }
        DoorButton("Open Out-door", busy) { vm.openOutdoor() }
        status?.let { Text(it) }
    }
}

@Composable
private fun DoorButton(label: String, busy: Boolean, onClick: () -> Unit) {
    Button(onClick, enabled = !busy, modifier = Modifier.fillMaxWidth().height(96.dp)) {
        if (busy) CircularProgressIndicator(Modifier.size(24.dp)) else Text(label)
    }
}
