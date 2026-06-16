// Unit tests for Go Mode service-instance settings.
package com.fghbuild.gomode.data

import androidx.datastore.preferences.core.PreferenceDataStoreFactory
import kotlinx.coroutines.TimeoutCancellationException
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.File

class SettingsRepositoryTest {
    // Deadlock guard around inherently-async DataStore reads. StateFlow makes these waits
    // eventually correct, so this only fires on a real hang; keep generous headroom so a
    // cold first read on a saturated CI runner does not flake.
    private val settleTimeoutMs = 10_000L

    private fun createRepo(): SettingsRepository {
        val file = File.createTempFile("gomode_test_prefs", ".preferences_pb")
        val dataStore = PreferenceDataStoreFactory.create { file }
        return SettingsRepository(dataStore)
    }

    // Awaits the first settings state matching [predicate]. On timeout, surfaces what we were
    // waiting for and the last observed StateFlow value so a CI flake is diagnosable.
    private suspend fun SettingsRepository.awaitSettings(
        waitingFor: String,
        predicate: (SettingsState) -> Boolean = { true },
    ): SettingsState =
        try {
            withTimeout(settleTimeoutMs) { settings.first(predicate) }
        } catch (e: TimeoutCancellationException) {
            throw AssertionError("Timed out after ${settleTimeoutMs}ms awaiting $waitingFor; last state = ${settings.value}", e)
        }

    @Test
    fun `initial settings have no active service`() = runBlocking {
        val repo = createRepo()
        val state = repo.awaitSettings("any initial state")

        assertTrue(state.services.isEmpty())
        assertEquals("", state.activeServiceURL)
        assertEquals("", state.activeServiceId)
        assertNull(state.haloAddress)
        assertFalse(state.haloAutoConnect)
    }

    @Test
    fun `saveActiveService creates active web service`() = runBlocking {
        val repo = createRepo()
        val id = repo.saveActiveService(label = "Local", url = "http://localhost:2242/")
        val state = repo.awaitSettings("activeServiceURL non-blank") { it.activeServiceURL.isNotBlank() }

        assertEquals(id, state.activeServiceId)
        assertEquals("Local", state.services.single().label)
        assertEquals("web", state.services.single().kind)
        assertEquals("http://localhost:2242", state.activeServiceURL)
    }

    @Test
    fun `saveActiveService updates active service`() = runBlocking {
        val repo = createRepo()
        val id = repo.saveActiveService(label = "Local", url = "http://localhost:2242")
        repo.saveActiveService(label = "Home", url = "https://example.com/")
        val state = repo.awaitSettings("activeServiceURL == https://example.com") { it.activeServiceURL == "https://example.com" }

        assertEquals(id, state.activeServiceId)
        assertEquals(1, state.services.size)
        assertEquals("Home", state.services.single().label)
    }

    @Test
    fun `switchService ignores unknown service id`() = runBlocking {
        val repo = createRepo()
        val id = repo.saveActiveService(label = "Local", url = "http://localhost:2242")
        repo.switchService("missing")
        val state = repo.awaitSettings("activeServiceId == $id") { it.activeServiceId == id }

        assertEquals(id, state.activeServiceId)
    }

    @Test
    fun `updateHaloAddress stores and clears address`() = runBlocking {
        val repo = createRepo()
        repo.updateHaloAddress("AA:BB:CC:DD:EE:FF")
        val stored = repo.awaitSettings("haloAddress != null") { it.haloAddress != null }

        assertEquals("AA:BB:CC:DD:EE:FF", stored.haloAddress)

        repo.updateHaloAddress(null)
        val cleared = repo.awaitSettings("haloAddress == null") { it.haloAddress == null }

        assertNull(cleared.haloAddress)
    }

    @Test
    fun `updateHaloAutoConnect toggles flag`() = runBlocking {
        val repo = createRepo()
        repo.updateHaloAutoConnect(true)
        val state = repo.awaitSettings("haloAutoConnect == true") { it.haloAutoConnect }

        assertTrue(state.haloAutoConnect)
    }

    @Test
    fun `normalizeURL trims whitespace and trailing slash`() {
        assertEquals("http://localhost:2242", SettingsRepository.normalizeURL(" http://localhost:2242/ "))
    }
}
