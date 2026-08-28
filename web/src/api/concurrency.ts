import { apiClient, pluginPath } from "./client";
import type { ConcurrencyConfig, ConcurrencyPayload } from "../types";

export async function fetchConcurrency(): Promise<ConcurrencyPayload> {
  const client = apiClient();
  const { data } = await client.get<ConcurrencyPayload>(pluginPath("/concurrency"));
  return data;
}

export async function updateConcurrency(
  config: ConcurrencyConfig,
): Promise<ConcurrencyPayload> {
  const client = apiClient();
  const { data } = await client.put<ConcurrencyPayload>(
    pluginPath("/concurrency"),
    config,
  );
  return data;
}
