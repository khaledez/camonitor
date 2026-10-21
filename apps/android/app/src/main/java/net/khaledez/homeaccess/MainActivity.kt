package net.khaledez.homeaccess

import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.compose.material3.MaterialTheme
import androidx.compose.runtime.getValue
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import androidx.lifecycle.viewmodel.initializer
import androidx.lifecycle.viewmodel.viewModelFactory

class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        val store = KeystoreSessionStore(applicationContext)
        setContent {
            MaterialTheme {
                val vm: GuestViewModel = viewModel(
                    factory = viewModelFactory { initializer { GuestViewModel(store) } },
                )
                val phase by vm.phase.collectAsStateWithLifecycle()
                when (phase) {
                    Phase.NeedsEnrollment -> EnrollScreen(vm)
                    Phase.Enrolled -> HomeScreen(vm)
                }
            }
        }
    }
}
