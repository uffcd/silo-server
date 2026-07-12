import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, render } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { adminKeys, catalogKeys, libraryKeys, recKeys, sectionKeys } from "@/hooks/queries/keys";
import {
  flushCatalogInvalidation,
  invalidateCatalogState,
  scheduleCatalogInvalidation,
} from "./realtimeCatalogInvalidation";
import { buildEventsUrl, RealtimeEventsProvider } from "./RealtimeEventsProvider";

const mockState = vi.hoisted(() => ({
  user: {
    id: 1,
    username: "admin",
    email: "admin@example.com",
    role: "admin",
    permissions: [],
    download_allowed: true,
  },
  pageActivity: {
    isVisible: true,
    isFocused: true,
    isFrozen: false,
    canPollDashboard: true,
    canApplyRealtimeUpdates: true,
  },
}));

vi.mock("@/hooks/useAuth", () => {
  const useAuth = () => ({
    user: mockState.user,
    profile: null,
  });
  return { useAuth, useOptionalAuth: useAuth };
});

vi.mock("@/hooks/usePageActivity", () => ({
  usePageActivity: () => mockState.pageActivity,
}));

vi.mock("react-router", () => ({
  useLocation: () => ({ pathname: "/" }),
}));

class FakeWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 3;
  static instances: FakeWebSocket[] = [];

  onopen: (() => void) | null = null;
  onmessage: ((event: MessageEvent) => void) | null = null;
  onerror: (() => void) | null = null;
  onclose: (() => void) | null = null;
  readyState = FakeWebSocket.CONNECTING;

  constructor(public url: string) {
    FakeWebSocket.instances.push(this);
  }

  send() {}

  close() {
    this.readyState = FakeWebSocket.CLOSED;
  }

  emitClose() {
    this.readyState = FakeWebSocket.CLOSED;
    this.onclose?.();
  }
}

describe("buildEventsUrl", () => {
  it("includes auth token and websocket scheme", () => {
    expect(
      buildEventsUrl("token-123", {
        protocol: "https:",
        host: "example.com",
      }),
    ).toBe("wss://example.com/api/v1/events/ws?token=token-123");
  });

  it("omits the query string when no token is available", () => {
    expect(
      buildEventsUrl(null, {
        protocol: "http:",
        host: "localhost:5173",
      }),
    ).toBe("ws://localhost:5173/api/v1/events/ws");
  });
});

describe("invalidateCatalogState", () => {
  it("invalidates library lists for a scoped library change", async () => {
    const queryClient = new QueryClient();
    const otherCatalogKey = catalogKeys.list({
      source: "section",
      scope: "library",
      section_id: "all",
      library_id: 1,
      limit: 60,
      offset: 0,
    });
    const changedCatalogKey = catalogKeys.list({
      source: "section",
      scope: "library",
      section_id: "all",
      library_id: 3,
      limit: 60,
      offset: 0,
    });
    const otherSectionKey = sectionKeys.libraryLayout(1);
    const changedSectionKey = sectionKeys.libraryLayout(3);
    const userLibrariesKey = libraryKeys.user("profile-1");

    queryClient.setQueryData(adminKeys.libraries(), []);
    queryClient.setQueryData(adminKeys.libraryMatchQueueStatuses(), []);
    queryClient.setQueryData(userLibrariesKey, []);
    queryClient.setQueryData(otherCatalogKey, { items: [] });
    queryClient.setQueryData(changedCatalogKey, { items: [] });
    queryClient.setQueryData(otherSectionKey, { sections: [] });
    queryClient.setQueryData(changedSectionKey, { sections: [] });

    invalidateCatalogState(queryClient, { libraryId: 3, allowDashboardRefetch: false });
    await Promise.resolve();

    expect(queryClient.getQueryState(adminKeys.libraries())?.isInvalidated).toBe(true);
    expect(queryClient.getQueryState(adminKeys.libraryMatchQueueStatuses())?.isInvalidated).toBe(
      true,
    );
    expect(queryClient.getQueryState(userLibrariesKey)?.isInvalidated).toBe(true);
    expect(queryClient.getQueryState(otherCatalogKey)?.isInvalidated).toBe(false);
    expect(queryClient.getQueryState(changedCatalogKey)?.isInvalidated).toBe(true);
    expect(queryClient.getQueryState(otherSectionKey)?.isInvalidated).toBe(false);
    expect(queryClient.getQueryState(changedSectionKey)?.isInvalidated).toBe(true);
  });

  it("can skip library lists for item-scoped catalog changes", async () => {
    const queryClient = new QueryClient();
    const changedCatalogKey = catalogKeys.list({
      source: "section",
      scope: "library",
      section_id: "all",
      library_id: 3,
      limit: 60,
      offset: 0,
    });

    queryClient.setQueryData(adminKeys.libraries(), []);
    queryClient.setQueryData(adminKeys.libraryMatchQueueStatuses(), []);
    queryClient.setQueryData(libraryKeys.all, []);
    queryClient.setQueryData(changedCatalogKey, { items: [] });

    invalidateCatalogState(queryClient, {
      itemId: "item-1",
      libraryId: 3,
      allowDashboardRefetch: false,
      includeLibraryLists: false,
    });
    await Promise.resolve();

    expect(queryClient.getQueryState(adminKeys.libraries())?.isInvalidated).toBe(false);
    expect(queryClient.getQueryState(adminKeys.libraryMatchQueueStatuses())?.isInvalidated).toBe(
      false,
    );
    expect(queryClient.getQueryState(libraryKeys.all)?.isInvalidated).toBe(false);
    expect(queryClient.getQueryState(changedCatalogKey)?.isInvalidated).toBe(true);
  });
});

describe("scheduleCatalogInvalidation", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  // adminKeys.stats() is invalidated exactly once per flush, so its
  // invalidateQueries call count works as a flush counter.
  function countStatsFlushes(spy: ReturnType<typeof vi.spyOn>) {
    return spy.mock.calls.filter(
      (call: unknown[]) =>
        JSON.stringify((call[0] as { queryKey?: unknown })?.queryKey) ===
        JSON.stringify(adminKeys.stats()),
    ).length;
  }

  it("coalesces a burst of events into a single flush after the debounce window", () => {
    const queryClient = new QueryClient();
    queryClient.setQueryData(recKeys.forYouMain(), { row: null });
    const spy = vi.spyOn(queryClient, "invalidateQueries");

    for (let i = 0; i < 5; i++) {
      scheduleCatalogInvalidation(queryClient, {
        itemId: `item-${i}`,
        allowDashboardRefetch: false,
        includeLibraryLists: false,
      });
      vi.advanceTimersByTime(500);
    }

    expect(queryClient.getQueryState(recKeys.forYouMain())?.isInvalidated).toBe(false);
    expect(countStatsFlushes(spy)).toBe(0);

    vi.advanceTimersByTime(3_000);

    expect(queryClient.getQueryState(recKeys.forYouMain())?.isInvalidated).toBe(true);
    expect(countStatsFlushes(spy)).toBe(1);
  });

  it("flushes at the max-wait even under a continuous event stream", () => {
    const queryClient = new QueryClient();
    const spy = vi.spyOn(queryClient, "invalidateQueries");

    // An event every 2s never lets the 3s debounce fire on its own.
    for (let elapsed = 0; elapsed <= 16_000; elapsed += 2_000) {
      scheduleCatalogInvalidation(queryClient, {
        allowDashboardRefetch: false,
        includeLibraryLists: false,
      });
      if (elapsed < 14_000) {
        expect(countStatsFlushes(spy)).toBe(0);
      }
      vi.advanceTimersByTime(2_000);
    }

    expect(countStatsFlushes(spy)).toBeGreaterThanOrEqual(1);
  });

  it("keeps a single library scoped and widens to match-all for mixed libraries", () => {
    const makeKeys = () => {
      const catalogKeyFor = (libraryId: number) =>
        catalogKeys.list({
          source: "section",
          scope: "library",
          section_id: "all",
          library_id: libraryId,
          limit: 60,
          offset: 0,
        });
      return { lib1: catalogKeyFor(1), lib3: catalogKeyFor(3) };
    };

    // Single library id: invalidation stays scoped.
    let queryClient = new QueryClient();
    let keys = makeKeys();
    queryClient.setQueryData(keys.lib1, { items: [] });
    queryClient.setQueryData(keys.lib3, { items: [] });
    scheduleCatalogInvalidation(queryClient, { libraryId: 3, allowDashboardRefetch: false });
    flushCatalogInvalidation(queryClient);
    expect(queryClient.getQueryState(keys.lib1)?.isInvalidated).toBe(false);
    expect(queryClient.getQueryState(keys.lib3)?.isInvalidated).toBe(true);

    // Two distinct library ids merge into an unscoped (match-all) flush.
    queryClient = new QueryClient();
    keys = makeKeys();
    queryClient.setQueryData(keys.lib1, { items: [] });
    queryClient.setQueryData(keys.lib3, { items: [] });
    scheduleCatalogInvalidation(queryClient, { libraryId: 1, allowDashboardRefetch: false });
    scheduleCatalogInvalidation(queryClient, { libraryId: 3, allowDashboardRefetch: false });
    flushCatalogInvalidation(queryClient);
    expect(queryClient.getQueryState(keys.lib1)?.isInvalidated).toBe(true);
    expect(queryClient.getQueryState(keys.lib3)?.isInvalidated).toBe(true);
  });

  it("invalidates the detail queries of every queued item on flush", () => {
    const queryClient = new QueryClient();
    queryClient.setQueryData(catalogKeys.itemDetail("item-a"), { content_id: "item-a" });
    queryClient.setQueryData(catalogKeys.itemDetail("item-b"), { content_id: "item-b" });

    scheduleCatalogInvalidation(queryClient, {
      itemId: "item-a",
      allowDashboardRefetch: false,
      includeLibraryLists: false,
    });
    scheduleCatalogInvalidation(queryClient, {
      itemId: "item-b",
      allowDashboardRefetch: false,
      includeLibraryLists: false,
    });
    flushCatalogInvalidation(queryClient);

    expect(queryClient.getQueryState(catalogKeys.itemDetail("item-a"))?.isInvalidated).toBe(true);
    expect(queryClient.getQueryState(catalogKeys.itemDetail("item-b"))?.isInvalidated).toBe(true);
  });

  it("merges includeLibraryLists across queued events", () => {
    const queryClient = new QueryClient();
    queryClient.setQueryData(libraryKeys.all, []);

    scheduleCatalogInvalidation(queryClient, {
      itemId: "item-a",
      allowDashboardRefetch: false,
      includeLibraryLists: false,
    });
    scheduleCatalogInvalidation(queryClient, { libraryId: 3, allowDashboardRefetch: false });
    flushCatalogInvalidation(queryClient);

    expect(queryClient.getQueryState(libraryKeys.all)?.isInvalidated).toBe(true);
  });
});

describe("RealtimeEventsProvider", () => {
  beforeEach(() => {
    FakeWebSocket.instances = [];
    vi.useFakeTimers();
    vi.stubGlobal("WebSocket", FakeWebSocket);
    mockState.pageActivity = {
      isVisible: true,
      isFocused: true,
      isFrozen: false,
      canPollDashboard: true,
      canApplyRealtimeUpdates: true,
    };
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it("ignores stale close events from intentionally closed sockets", () => {
    const queryClient = new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    });
    const view = render(
      <QueryClientProvider client={queryClient}>
        <RealtimeEventsProvider>
          <div />
        </RealtimeEventsProvider>
      </QueryClientProvider>,
    );

    expect(FakeWebSocket.instances).toHaveLength(1);
    const firstSocket = FakeWebSocket.instances[0];

    act(() => {
      mockState.pageActivity = {
        ...mockState.pageActivity,
        canApplyRealtimeUpdates: false,
      };
      view.rerender(
        <QueryClientProvider client={queryClient}>
          <RealtimeEventsProvider>
            <div />
          </RealtimeEventsProvider>
        </QueryClientProvider>,
      );
    });

    act(() => {
      mockState.pageActivity = {
        ...mockState.pageActivity,
        canApplyRealtimeUpdates: true,
      };
      view.rerender(
        <QueryClientProvider client={queryClient}>
          <RealtimeEventsProvider>
            <div />
          </RealtimeEventsProvider>
        </QueryClientProvider>,
      );
    });

    expect(FakeWebSocket.instances).toHaveLength(2);

    act(() => {
      firstSocket?.emitClose();
      vi.advanceTimersByTime(1_000);
    });

    expect(FakeWebSocket.instances).toHaveLength(2);
  });

  it("defers known catalog events and ignores unknown ones", () => {
    const queryClient = new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    });
    queryClient.setQueryData(recKeys.similar("movie-1"), { items: [] });
    render(
      <QueryClientProvider client={queryClient}>
        <RealtimeEventsProvider>
          <div />
        </RealtimeEventsProvider>
      </QueryClientProvider>,
    );

    const socket = FakeWebSocket.instances[0]!;
    const sendCatalogEvent = (eventName: string) => {
      act(() => {
        socket.onmessage?.({
          data: JSON.stringify({
            type: "event",
            channel: "catalog",
            event: eventName,
            event_id: "evt-1",
            timestamp: "2026-01-01T00:00:00Z",
            data: { content_id: "movie-1" },
          }),
        } as MessageEvent);
      });
    };

    // Unknown event names are ignored entirely — no deferred invalidation.
    sendCatalogEvent("catalog.some.future.event");
    act(() => {
      vi.advanceTimersByTime(20_000);
    });
    expect(queryClient.getQueryState(recKeys.similar("movie-1"))?.isInvalidated).toBe(false);

    // A known event defers its invalidation to the debounce flush.
    sendCatalogEvent("metadata.updated");
    expect(queryClient.getQueryState(recKeys.similar("movie-1"))?.isInvalidated).toBe(false);
    act(() => {
      vi.advanceTimersByTime(3_000);
    });
    expect(queryClient.getQueryState(recKeys.similar("movie-1"))?.isInvalidated).toBe(true);
  });
});
