import { useEffect, useMemo, useState } from "react";
import { useNavigate, useLocation } from "react-router-dom";
import { bindNativeKey, createKey, fetchStatus, listCPANativeKeys, maskCPAKey } from "../api/keys";
import KeyForm from "../components/KeyForm";
import PlainKeyModal from "../components/PlainKeyModal";
import { MobileFormHeader, MobileTabBar } from "./KeyList";
import { useT } from "../i18n";
import type { KeyPublic, ModelRule } from "../types";

export default function KeyNew() {
  const nav = useNavigate();
  const loc = useLocation();
  const t = useT();
  const [plain, setPlain] = useState<string | null>(null);
  const [authMode, setAuthMode] = useState<"plugin" | "cpa-native" | null>(null);
  const [nativeKeys, setNativeKeys] = useState<string[]>([]);
  const [selectedNativeKey, setSelectedNativeKey] = useState("");
  const [loadError, setLoadError] = useState("");

  const title = authMode === "cpa-native" ? t("new.bindTitle") : t("new.title");

  useEffect(() => {
    let alive = true;
    void (async () => {
      try {
        const status = await fetchStatus();
        if (!alive) return;
        setAuthMode(status.auth_mode ?? "plugin");
        if (status.auth_mode === "cpa-native") {
          const keys = await listCPANativeKeys();
          if (!alive) return;
          setNativeKeys(keys);
          setSelectedNativeKey(keys[0] ?? "");
        }
      } catch (error) {
        if (!alive) return;
        setLoadError((error as Error).message ?? t("new.nativeLoadFailed"));
      }
    })();
    return () => { alive = false; };
  }, [t]);

  // When the standalone model-picker page returns here with a selection,
  // merge it into the form's initial models. Pricing rows for newly-picked
  // aliases start at 0; preserved aliases keep their existing rows via
  // KeyForm's price-map init from `initial.models`.
  const picked = (loc.state as { pickedModels?: ModelRule[] } | null)?.pickedModels;
  const initial = useMemo<KeyPublic | undefined>(
    () => (picked ? ({ id: "", name: "", enabled: true, rpm: 0, models: picked, daily_limit_usd: 0, weekly_limit_usd: 0 } as KeyPublic) : undefined),
    [picked],
  );

  if (authMode === null && !loadError) return <div className="muted">{t("keys.loading")}</div>;
  if (loadError) return <div className="error">{loadError}</div>;

  return (
    <div className="form-page">
      <div className="fp-head mobile-hidden">
        <h1>{title}</h1>
      </div>
      <MobileFormHeader title={title} backTo="/keys" />
      {authMode === "cpa-native" && (
        <div className="card native-key-picker">
          <label htmlFor="native-key-select">{t("new.selectNativeKey")}</label>
          <select
            id="native-key-select"
            className="input"
            value={selectedNativeKey}
            onChange={(event) => setSelectedNativeKey(event.target.value)}
          >
            {nativeKeys.length === 0 && <option value="">{t("new.noNativeKeys")}</option>}
            {nativeKeys.map((key) => <option key={key} value={key}>{maskCPAKey(key)}</option>)}
          </select>
          <p className="muted">{t("new.nativeKeyNote")}</p>
        </div>
      )}
      <KeyForm
        initial={initial}
        idOptional={authMode === "cpa-native"}
        pickPath="/keys/new/models"
        submitLabel={t("new.create")}
        onCancel={() => nav("/keys")}
        onSubmit={async (v) => {
          if (authMode === "cpa-native") {
            if (!selectedNativeKey) throw new Error(t("new.nativeKeyRequired"));
            await bindNativeKey({
              id: v.id || undefined,
              key: selectedNativeKey,
              name: v.name || undefined,
              enabled: v.enabled,
              rpm: v.rpm,
              models: v.models,
              daily_limit_usd: v.daily_limit_usd,
              weekly_limit_usd: v.weekly_limit_usd,
              allow_models_endpoint: v.allow_models_endpoint,
              allow_all_models: v.allow_all_models,
            });
            nav("/keys");
            return;
          }
          const r = await createKey({
            id: v.id,
            name: v.name || undefined,
            enabled: v.enabled,
            rpm: v.rpm,
            models: v.models,
            daily_limit_usd: v.daily_limit_usd,
            weekly_limit_usd: v.weekly_limit_usd,
            allow_models_endpoint: v.allow_models_endpoint,
            allow_all_models: v.allow_all_models,
          });
          setPlain(r.plain_key);
        }}
      />
      {authMode !== "cpa-native" && <p className="fp-note mobile-hidden">{t("login.memoryNote")}</p>}
      {plain && (
        <PlainKeyModal
          plainKey={plain}
          title={t("plainModal.created")}
          onClose={() => {
            setPlain(null);
            nav("/keys");
          }}
        />
      )}
      <MobileTabBar active="new" />
    </div>
  );
}
