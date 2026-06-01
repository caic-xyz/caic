// Unit tests for SettingsRepository: server CRUD and preference management.
package com.fghbuild.caic.data

import androidx.datastore.preferences.core.PreferenceDataStoreFactory
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ExperimentalCoroutinesApi
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.test.UnconfinedTestDispatcher
import kotlinx.coroutines.test.resetMain
import kotlinx.coroutines.test.setMain
import kotlinx.coroutines.withTimeout
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import java.io.File

@OptIn(ExperimentalCoroutinesApi::class)
@RunWith(RobolectricTestRunner::class)
class SettingsRepositoryTest {

    private val testDispatcher = UnconfinedTestDispatcher()

    @Before
    fun setUp() {
        Dispatchers.setMain(testDispatcher)
    }

    @After
    fun tearDown() {
        Dispatchers.resetMain()
    }

    private fun createRepo(): SettingsRepository {
        val file = File.createTempFile("test_prefs", ".preferences_pb")
        val dataStore = PreferenceDataStoreFactory.create { file }
        return SettingsRepository(dataStore)
    }

    @Test
    fun `initial settings have no servers`() = runBlocking {
        val repo = createRepo()
        val state = withTimeout(5000) { repo.settings.first() }
        assertTrue(state.servers.isEmpty())
        assertEquals("", state.serverURL)
        assertEquals("", state.activeServerId)
    }

    @Test
    fun `initial settings have default voice values`() = runBlocking {
        val repo = createRepo()
        val state = withTimeout(5000) { repo.settings.first() }
        assertTrue(state.voiceEnabled)
        assertEquals("Orus", state.voiceName)
    }

    @Test
    fun `addServer creates server and makes it active`() = runBlocking {
        val repo = createRepo()
        val id = repo.addServer("My Server")
        assertTrue(id.isNotBlank())

        val state = withTimeout(5000) { repo.settings.first { it.servers.isNotEmpty() } }
        assertEquals(1, state.servers.size)
        assertEquals("My Server", state.servers[0].label)
        assertEquals(id, state.servers[0].id)
        assertEquals(id, state.activeServerId)
    }

    @Test
    fun `addServer with blank label defaults to Server N`() = runBlocking {
        val repo = createRepo()
        repo.addServer("")
        val state = withTimeout(5000) { repo.settings.first { it.servers.isNotEmpty() } }
        assertEquals("Server 1", state.servers[0].label)
    }

    @Test
    fun `addServer increments default label counter`() = runBlocking {
        val repo = createRepo()
        repo.addServer("")
        withTimeout(5000) { repo.settings.first { it.servers.size == 1 } }
        repo.addServer("")
        val state2 = withTimeout(5000) { repo.settings.first { it.servers.size == 2 } }
        assertEquals(2, state2.servers.size)
        assertEquals("Server 1", state2.servers[0].label)
        assertEquals("Server 2", state2.servers[1].label)
    }

    @Test
    fun `removeServer deletes and switches active`() = runBlocking {
        val repo = createRepo()
        val id1 = repo.addServer("Server 1")
        val id2 = repo.addServer("Server 2")

        // Both exist, id2 is active (last added).
        var state = withTimeout(5000) { repo.settings.first { it.servers.size == 2 } }
        assertEquals(2, state.servers.size)
        assertEquals(id2, state.activeServerId)

        // Remove id2 — active switches to id1.
        repo.removeServer(id2)
        state = withTimeout(5000) { repo.settings.first { it.servers.size == 1 } }
        assertEquals(1, state.servers.size)
        assertEquals(id1, state.activeServerId)
    }

    @Test
    fun `removeServer clears active id when last server removed`() = runBlocking {
        val repo = createRepo()
        val id = repo.addServer("Server 1")
        withTimeout(5000) { repo.settings.first { it.servers.isNotEmpty() } }
        repo.removeServer(id)
        val state = withTimeout(5000) { repo.settings.first { it.servers.isEmpty() } }
        assertEquals(0, state.servers.size)
        assertEquals("", state.activeServerId)
    }

    @Test
    fun `removeServer does nothing for unknown id`() = runBlocking {
        val repo = createRepo()
        val id = repo.addServer("Server 1")
        repo.removeServer("bogus")
        val state = withTimeout(5000) { repo.settings.first { it.servers.isNotEmpty() } }
        assertEquals(1, state.servers.size)
        assertEquals(id, state.activeServerId)
    }

    @Test
    fun `updateServerURL trims trailing slash`() = runBlocking {
        val repo = createRepo()
        repo.addServer("S1")
        repo.updateServerURL("http://example.com/")
        val state = withTimeout(5000) { repo.settings.first { it.serverURL.isNotBlank() } }
        assertEquals("http://example.com", state.serverURL)
    }

    @Test
    fun `updateServerURL persists without trailing slash`() = runBlocking {
        val repo = createRepo()
        val id = repo.addServer("S1")
        repo.updateServerURL("http://example.com")
        val state = withTimeout(5000) { repo.settings.first { it.serverURL.isNotBlank() } }
        assertEquals("http://example.com", state.serverURL)
    }

    @Test
    fun `updateServerLabel changes active server label`() = runBlocking {
        val repo = createRepo()
        repo.addServer("Old")
        repo.updateServerLabel("New")
        val state = withTimeout(5000) { repo.settings.first { it.servers.isNotEmpty() } }
        assertEquals("New", state.servers[0].label)
    }

    @Test
    fun `updateVoiceEnabled toggles flag`() = runBlocking {
        val repo = createRepo()
        repo.updateVoiceEnabled(false)
        val state = withTimeout(5000) { repo.settings.first { !it.voiceEnabled } }
        assertFalse(state.voiceEnabled)
    }

    @Test
    fun `updateVoiceName changes value`() = runBlocking {
        val repo = createRepo()
        repo.updateVoiceName("Zephyr")
        val state = withTimeout(5000) { repo.settings.first { it.voiceName == "Zephyr" } }
        assertEquals("Zephyr", state.voiceName)
    }

    @Test
    fun `updateAuthToken stores and retrieves token`() = runBlocking {
        val repo = createRepo()
        repo.addServer("S1")
        repo.updateAuthToken("secret-token")
        val state = withTimeout(5000) { repo.settings.first { it.authToken != null } }
        assertEquals("secret-token", state.authToken)
    }

    @Test
    fun `updateAuthToken with null clears token`() = runBlocking {
        val repo = createRepo()
        repo.addServer("S1")
        repo.updateAuthToken("secret")
        withTimeout(5000) { repo.settings.first { it.authToken == "secret" } }
        repo.updateAuthToken(null)
        val state = withTimeout(5000) { repo.settings.first { it.authToken == null } }
        assertNull(state.authToken)
    }

    @Test
    fun `switchServer changes active server`() = runBlocking {
        val repo = createRepo()
        val id1 = repo.addServer("S1")
        val id2 = repo.addServer("S2")
        withTimeout(5000) { repo.settings.first { it.servers.size == 2 } }

        // id2 is active (last added)
        assertEquals(id2, repo.settings.first().activeServerId)

        repo.switchServer(id1)
        val state = withTimeout(5000) { repo.settings.first { it.activeServerId == id1 } }
        assertEquals(id1, state.activeServerId)
    }

    @Test
    fun `active server URL is reflected in settings state`() = runBlocking {
        val repo = createRepo()
        val id1 = repo.addServer("S1")
        val id2 = repo.addServer("S2")
        repo.updateServerURL("http://s2.example.com")
        withTimeout(5000) { repo.settings.first { it.serverURL == "http://s2.example.com" } }

        // Switch to id1, set its URL
        repo.switchServer(id1)
        repo.updateServerURL("http://s1.example.com")
        val s1State = withTimeout(5000) { repo.settings.first { it.serverURL == "http://s1.example.com" } }
        assertEquals("http://s1.example.com", s1State.serverURL)

        // Switch back to id2 — URL should reflect id2
        repo.switchServer(id2)
        val s2State = withTimeout(5000) { repo.settings.first { it.serverURL == "http://s2.example.com" } }
        assertEquals("http://s2.example.com", s2State.serverURL)
    }

    @Test
    fun `updateServerPreferences stores and retrieves prefs`() = runBlocking {
        val repo = createRepo()
        assertNull(repo.serverPreferences.value)

        val prefs = com.caic.sdk.v1.PreferencesResp(
            repositories = emptyList(),
            harness = null,
            models = emptyMap(),
            settings = com.caic.sdk.v1.UserSettings(
                autoFixOnCIFailure = true, autoFixOnPROpen = true, useDefaultCaches = true,
            ),
        )
        repo.updateServerPreferences(prefs)
        assertNotNull(repo.serverPreferences.value)
        assertEquals(prefs, repo.serverPreferences.value)
    }

    @Test
    fun `updateServerPreferences null clears prefs`() = runBlocking {
        val repo = createRepo()
        repo.updateServerPreferences(
            com.caic.sdk.v1.PreferencesResp(
                repositories = emptyList(), harness = null, models = emptyMap(),
                settings = com.caic.sdk.v1.UserSettings(
                    autoFixOnCIFailure = false, autoFixOnPROpen = false, useDefaultCaches = false,
                ),
            ),
        )
        repo.updateServerPreferences(null)
        assertNull(repo.serverPreferences.value)
    }
}
