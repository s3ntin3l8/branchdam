import { afterEach, describe, expect, it, vi } from "vitest";
import { api, ApiError } from "./client";

function mockFetchOnce(status: number, body: unknown, ok = status >= 200 && status < 300) {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValue({
      ok,
      status,
      statusText: "test-status",
      json: async () => body,
    }),
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("api client", () => {
  it("returns parsed JSON on success", async () => {
    mockFetchOnce(200, { version: "1.2.3" });
    const result = await api.config();
    expect(result).toEqual({ version: "1.2.3" });
  });

  it("throws ApiError with the response detail on failure", async () => {
    mockFetchOnce(404, { detail: "asset not found" }, false);
    await expect(api.getAsset(999)).rejects.toMatchObject(
      new ApiError(404, "asset not found"),
    );
  });

  it("falls back to statusText when the error body isn't JSON", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue({
        ok: false,
        status: 500,
        statusText: "Internal Server Error",
        json: async () => {
          throw new Error("not json");
        },
      }),
    );
    await expect(api.config()).rejects.toMatchObject({ status: 500, message: "Internal Server Error" });
  });

  it("listAssets builds query params only for provided values", async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ assets: [] }),
    });
    vi.stubGlobal("fetch", fetchMock);

    await api.listAssets({ limit: 25 });
    const calledUrl = fetchMock.mock.calls[0][0] as string;
    expect(calledUrl).toContain("limit=25");
    expect(calledUrl).not.toContain("offset");
  });

  it("startScan sends the storageLocationId as a JSON body", async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ jobId: 7 }),
    });
    vi.stubGlobal("fetch", fetchMock);

    const result = await api.startScan(3);
    expect(result).toEqual({ jobId: 7 });
    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(JSON.parse(init.body as string)).toEqual({ storageLocationId: 3 });
    expect(init.method).toBe("POST");
  });
});
