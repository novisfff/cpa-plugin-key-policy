import { beforeEach, describe, expect, it, vi } from "vitest";

const get = vi.fn();
const put = vi.fn();

vi.mock("./client", () => ({
  apiClient: () => ({ get, put }),
  pluginPath: (suffix: string) => "/v0/management/plugins/cpa-key-policy" + suffix,
}));

import { fetchConcurrency, updateConcurrency } from "./concurrency";
import type { ConcurrencyConfig, ConcurrencyPayload } from "../types";

const payload: ConcurrencyPayload = {
  config: {
    enabled: true,
    global_limit: 6,
    queue_timeout: "60s",
    max_queue_per_key: 32,
  },
  status: {
    enabled: true,
    global_limit: 6,
    global_running: 5,
    total_waiting: 3,
    active_principals: 3,
    principals: [],
  },
};

describe("concurrency API", () => {
  beforeEach(() => {
    get.mockReset();
    put.mockReset();
  });

  it("fetches the plugin concurrency endpoint", async () => {
    get.mockResolvedValue({ data: payload });

    await expect(fetchConcurrency()).resolves.toEqual(payload);
    expect(get).toHaveBeenCalledWith(
      "/v0/management/plugins/cpa-key-policy/concurrency",
    );
  });

  it("puts the complete config and returns refreshed status", async () => {
    const config: ConcurrencyConfig = {
      enabled: true,
      global_limit: 4,
      queue_timeout: "45s",
      max_queue_per_key: 12,
    };
    const response = { ...payload, config };
    put.mockResolvedValue({ data: response });

    await expect(updateConcurrency(config)).resolves.toEqual(response);
    expect(put).toHaveBeenCalledWith(
      "/v0/management/plugins/cpa-key-policy/concurrency",
      config,
    );
  });
});
