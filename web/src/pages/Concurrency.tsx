import { useEffect, useState } from "react";
import { fetchConcurrency, updateConcurrency } from "../api/concurrency";
import type {
  ConcurrencyConfig,
  ConcurrencyRuntimeStatus,
} from "../types";
import { useT } from "../i18n";
import { MobileTabBar } from "./KeyList";

const DEFAULT_CONFIG: ConcurrencyConfig = {
  enabled: false,
  global_limit: 6,
  queue_timeout: "60s",
  max_queue_per_key: 32,
};

function errorMessage(error: unknown, fallback: string): string {
  const candidate = error as {
    response?: { data?: { error?: { message?: string } } };
    message?: string;
  };
  return candidate.response?.data?.error?.message ?? candidate.message ?? fallback;
}

export default function Concurrency() {
  const t = useT();
  const [config, setConfig] = useState<ConcurrencyConfig>(DEFAULT_CONFIG);
  const [status, setStatus] = useState<ConcurrencyRuntimeStatus | null>(null);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [saved, setSaved] = useState(false);

  useEffect(() => {
    let disposed = false;
    const loadInitial = async () => {
      try {
        const payload = await fetchConcurrency();
        if (disposed) return;
        setConfig(payload.config);
        setStatus(payload.status);
      } catch (cause) {
        if (!disposed) setError(errorMessage(cause, t("concurrency.loadFailed")));
      } finally {
        if (!disposed) setLoading(false);
      }
    };
    const pollStatus = async () => {
      try {
        const payload = await fetchConcurrency();
        if (!disposed) setStatus(payload.status);
      } catch {
        // Keep the last successful snapshot. A later poll or manual save will
        // recover without replacing the page with transient network noise.
      }
    };
    void loadInitial();
    const timer = window.setInterval(() => void pollStatus(), 3000);
    return () => {
      disposed = true;
      window.clearInterval(timer);
    };
  }, [t]);

  const save = async (event: React.FormEvent) => {
    event.preventDefault();
    setSaving(true);
    setSaved(false);
    setError("");
    try {
      const payload = await updateConcurrency(config);
      setConfig(payload.config);
      setStatus(payload.status);
      setSaved(true);
    } catch (cause) {
      setError(errorMessage(cause, t("concurrency.saveFailed")));
    } finally {
      setSaving(false);
    }
  };

  if (loading) {
    return <div className="muted concurrency-loading">{t("concurrency.loading")}</div>;
  }

  const running = status?.global_running ?? 0;
  const limit = status?.global_limit ?? config.global_limit;

  return (
    <div className="concurrency-page">
      <div className="fp-head concurrency-heading">
        <div>
          <h1>{t("concurrency.title")}</h1>
          <p className="muted">{t("concurrency.subtitle")}</p>
        </div>
        <span className={"tag " + (status?.enabled ? "on" : "off")}>
          {status?.enabled ? t("concurrency.enabled") : t("concurrency.disabled")}
        </span>
      </div>

      {error && <div className="error">{error}</div>}
      {saved && <div className="success">{t("concurrency.saved")}</div>}

      <div className="concurrency-layout">
        <form className="card concurrency-config" onSubmit={save}>
          <h2>{t("concurrency.configTitle")}</h2>
          <label className="concurrency-enable-row">
            <span>
              <strong>{t("concurrency.enableLabel")}</strong>
              <small>{t("concurrency.enableHint")}</small>
            </span>
            <input
              type="checkbox"
              checked={config.enabled}
              onChange={(event) => setConfig({ ...config, enabled: event.target.checked })}
            />
          </label>
          <div className="form-row">
            <label htmlFor="concurrency-global-limit">{t("concurrency.globalLimit")}</label>
            <input
              id="concurrency-global-limit"
              className="input"
              type="number"
              min={1}
              step={1}
              required
              value={config.global_limit}
              onChange={(event) => setConfig({ ...config, global_limit: Number(event.target.value) })}
            />
            <small className="muted">{t("concurrency.globalLimitHint")}</small>
          </div>
          <div className="form-row">
            <label htmlFor="concurrency-queue-timeout">{t("concurrency.queueTimeout")}</label>
            <input
              id="concurrency-queue-timeout"
              className="input"
              type="text"
              required
              placeholder="60s"
              value={config.queue_timeout}
              onChange={(event) => setConfig({ ...config, queue_timeout: event.target.value })}
            />
            <small className="muted">{t("concurrency.queueTimeoutHint")}</small>
          </div>
          <div className="form-row">
            <label htmlFor="concurrency-max-queue">{t("concurrency.maxQueue")}</label>
            <input
              id="concurrency-max-queue"
              className="input"
              type="number"
              min={1}
              step={1}
              required
              value={config.max_queue_per_key}
              onChange={(event) => setConfig({ ...config, max_queue_per_key: Number(event.target.value) })}
            />
            <small className="muted">{t("concurrency.maxQueueHint")}</small>
          </div>
          <button className="btn primary" type="submit" disabled={saving}>
            {saving ? t("concurrency.saving") : t("concurrency.save")}
          </button>
        </form>

        <section className="concurrency-runtime">
          <h2>{t("concurrency.runtimeTitle")}</h2>
          <div className="concurrency-metrics">
            <div className="card concurrency-metric">
              <span>{t("concurrency.running")}</span>
              <strong>{running} / {limit}</strong>
            </div>
            <div className="card concurrency-metric">
              <span>{t("concurrency.waiting")}</span>
              <strong>{status?.total_waiting ?? 0}</strong>
            </div>
            <div className="card concurrency-metric">
              <span>{t("concurrency.activeUsers")}</span>
              <strong>{status?.active_principals ?? 0}</strong>
            </div>
          </div>
          <div className="card concurrency-principals">
            <div className="concurrency-table-head">
              <strong>{t("concurrency.keyPrincipal")}</strong>
              <strong>{t("concurrency.running")}</strong>
              <strong>{t("concurrency.waiting")}</strong>
            </div>
            {(status?.principals.length ?? 0) === 0 ? (
              <div className="muted concurrency-empty">{t("concurrency.noActiveUsers")}</div>
            ) : status?.principals.map((principal) => (
              <div className="concurrency-principal-row" key={principal.principal}>
                <div>
                  <strong>{principal.key_name || principal.key_id || principal.principal}</strong>
                  <span className="mono">{principal.key_id || principal.principal}</span>
                  {principal.key_preview && <span className="muted mono">{principal.key_preview}</span>}
                </div>
                <strong>{principal.running}</strong>
                <strong>{principal.waiting}</strong>
              </div>
            ))}
          </div>
          <p className="muted concurrency-poll-note">{t("concurrency.pollingHint")}</p>
        </section>
      </div>

      <MobileTabBar active="concurrency" />
    </div>
  );
}
