// Unit tests for Go Mode service-instance settings.
package com.fghbuild.gomode.data

import androidx.datastore.preferences.core.PreferenceDataStoreFactory
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.File

class SettingsRepositoryTest {
    private fun createRepo(): SettingsRepository {
        val file = File.createTempFile("gomode_test_prefs", ".preferences_pb")
        val dataStore = PreferenceDataStoreFactory.create { file }
        return SettingsRepository(dataStore)
    }

    @Test
    fun `initial settings have no active service`() = runBlocking {
        val repo = createRepo()
        val state = withTimeout(5000) { repo.settings.first() }

        assertTrue(state.services.isEmpty())
        assertEquals("", state.activeServiceURL)
        assertEquals("", state.activeServiceId)
    }

    @Test
    fun `saveActiveService creates active web service`() = runBlocking {
        val repo = createRepo()
        val id = repo.saveActiveService(label = "Local", url = "http://localhost:2242/")
        val state = withTimeout(5000) { repo.settings.first { it.activeServiceURL.isNotBlank() } }

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
        val state = withTimeout(5000) { repo.settings.first { it.activeServiceURL == "https://example.com" } }

        assertEquals(id, state.activeServiceId)
        assertEquals(1, state.services.size)
        assertEquals("Home", state.services.single().label)
    }

    @Test
    fun `switchService ignores unknown service id`() = runBlocking {
        val repo = createRepo()
        val id = repo.saveActiveService(label = "Local", url = "http://localhost:2242")
        repo.switchService("missing")
        val state = withTimeout(5000) { repo.settings.first { it.activeServiceId == id } }

        assertEquals(id, state.activeServiceId)
    }

    @Test
    fun `normalizeURL trims whitespace and trailing slash`() {
        assertEquals("http://localhost:2242", SettingsRepository.normalizeURL(" http://localhost:2242/ "))
    }
}
