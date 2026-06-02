// Route pane for /settings — full-page application settings, fed from the app store.
import SettingsPage from "./SettingsPage";
import { useAppState } from "./AppState";
import styles from "./layout.module.css";

export default function SettingsPane() {
  const s = useAppState();
  return (
    <div class={styles.layout}>
      <SettingsPage
        selectedImage={s.selectedImage}
        setSelectedImage={s.setSelectedImage}
        maxCPUs={s.maxCPUs}
        setMaxCPUs={s.setMaxCPUs}
        wellKnownCaches={s.wellKnownCaches}
        setWellKnownCaches={s.setWellKnownCaches}
        wellKnownCachesList={s.wellKnownCachesList}
        cacheMappings={s.cacheMappings}
        setCacheMappings={s.setCacheMappings}
        autoFixCI={s.autoFixCI}
        setAutoFixCI={s.setAutoFixCI}
        autoFixPR={s.autoFixPR}
        setAutoFixPR={s.setAutoFixPR}
        versionInfo={s.versionInfo}
        versionCheckError={s.versionCheckError}
        checkingUpdate={s.checkingUpdate}
        updating={s.updating}
        updateStatus={s.updateStatus}
        saveSettings={s.saveSettings}
        triggerServerUpdate={s.triggerServerUpdate}
      />
    </div>
  );
}
