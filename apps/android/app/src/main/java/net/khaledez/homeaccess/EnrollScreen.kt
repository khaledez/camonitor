package net.khaledez.homeaccess

import android.Manifest
import android.content.pm.PackageManager
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.key
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp
import androidx.core.content.ContextCompat
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import kotlinx.coroutines.launch

@Composable
fun EnrollScreen(vm: GuestViewModel) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    val error by vm.enrollError.collectAsStateWithLifecycle()

    var cameraGranted by remember {
        mutableStateOf(
            ContextCompat.checkSelfPermission(context, Manifest.permission.CAMERA) ==
                PackageManager.PERMISSION_GRANTED,
        )
    }
    var permissionAsked by remember { mutableStateOf(false) }
    val launcher = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) {
        cameraGranted = it
        permissionAsked = true
    }
    LaunchedEffect(Unit) {
        if (!cameraGranted) launcher.launch(Manifest.permission.CAMERA)
    }

    // QrScanner fires once per instance, so a new key gives it a fresh shot after a failed scan.
    var scanAttempt by remember { mutableIntStateOf(0) }

    Column(
        Modifier.fillMaxSize().padding(24.dp),
        verticalArrangement = Arrangement.spacedBy(16.dp),
    ) {
        Text("Scan your invite QR code", style = MaterialTheme.typography.headlineSmall)
        error?.let { Text(it, color = MaterialTheme.colorScheme.error) }

        Box(Modifier.weight(1f).fillMaxSize()) {
            if (cameraGranted) {
                key(scanAttempt) {
                    QrScanner { raw ->
                        scope.launch {
                            vm.handleScanned(raw)
                            if (vm.phase.value == Phase.NeedsEnrollment) scanAttempt++
                        }
                    }
                }
            } else if (permissionAsked) {
                Text("Camera access needed — enable it in Settings")
            }
        }
    }
}
