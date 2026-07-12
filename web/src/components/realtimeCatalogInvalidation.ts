import type { QueryClient } from "@tanstack/react-query";
import { adminKeys, libraryKeys } from "@/hooks/queries/keys";
import { invalidateMediaSurfaceQueries } from "@/hooks/queries/mediaSurfaceRefresh";
import { bumpHomeRefreshSignal } from "@/pages/homeSurfaceRefresh";

interface CatalogInvalidationOptions {
  itemId?: string;
  libraryId?: number;
  allowDashboardRefetch: boolean;
  includeLibraryLists?: boolean;
}

export function invalidateCatalogState(
  queryClient: QueryClient,
  options: CatalogInvalidationOptions,
) {
  const { itemId, libraryId, allowDashboardRefetch, includeLibraryLists = true } = options;
  applyCatalogInvalidation(queryClient, {
    itemIds: itemId ? [itemId] : [],
    libraryId,
    allowDashboardRefetch,
    includeLibraryLists,
  });
}

// Realtime catalog/user_state events can arrive continuously (a library scan
// emits one per touched item), and every invalidation refetches all active
// media-surface queries regardless of staleTime. Scheduling instead of
// invalidating directly coalesces event bursts into one flush: trailing-edge
// debounce, with a max-wait so a steady event stream still refreshes
// periodically instead of never.
const DEBOUNCE_MS = 3_000;
const MAX_WAIT_MS = 15_000;

interface PendingInvalidation {
  itemIds: Set<string>;
  libraryIds: Set<number>;
  unscopedLibrary: boolean;
  includeLibraryLists: boolean;
  allowDashboardRefetch: boolean;
  timer: ReturnType<typeof setTimeout>;
  firstQueuedAt: number;
}

const pendingByClient = new WeakMap<QueryClient, PendingInvalidation>();

export function scheduleCatalogInvalidation(
  queryClient: QueryClient,
  options: CatalogInvalidationOptions,
) {
  const { itemId, libraryId, allowDashboardRefetch, includeLibraryLists = true } = options;

  const pending = pendingByClient.get(queryClient);
  if (pending) {
    if (itemId) pending.itemIds.add(itemId);
    if (libraryId) {
      pending.libraryIds.add(libraryId);
    } else {
      pending.unscopedLibrary = true;
    }
    pending.includeLibraryLists ||= includeLibraryLists;
    pending.allowDashboardRefetch ||= allowDashboardRefetch;

    // Trailing-edge debounce, capped so a continuous event stream flushes at
    // least every MAX_WAIT_MS.
    const remainingMaxWait = pending.firstQueuedAt + MAX_WAIT_MS - Date.now();
    clearTimeout(pending.timer);
    if (remainingMaxWait <= 0) {
      flushCatalogInvalidation(queryClient);
      return;
    }
    pending.timer = setTimeout(
      () => flushCatalogInvalidation(queryClient),
      Math.min(DEBOUNCE_MS, remainingMaxWait),
    );
    return;
  }

  pendingByClient.set(queryClient, {
    itemIds: new Set(itemId ? [itemId] : []),
    libraryIds: new Set(libraryId ? [libraryId] : []),
    unscopedLibrary: !libraryId,
    includeLibraryLists,
    allowDashboardRefetch,
    timer: setTimeout(() => flushCatalogInvalidation(queryClient), DEBOUNCE_MS),
    firstQueuedAt: Date.now(),
  });
}

/** Flushes any pending scheduled invalidation immediately. Exported for tests. */
export function flushCatalogInvalidation(queryClient: QueryClient) {
  const pending = pendingByClient.get(queryClient);
  if (!pending) return;
  pendingByClient.delete(queryClient);
  clearTimeout(pending.timer);

  applyCatalogInvalidation(queryClient, {
    itemIds: [...pending.itemIds],
    // A single scoped library keeps the invalidation narrow; anything mixed
    // or unscoped falls back to matching every library.
    libraryId:
      !pending.unscopedLibrary && pending.libraryIds.size === 1
        ? [...pending.libraryIds][0]
        : undefined,
    allowDashboardRefetch: pending.allowDashboardRefetch,
    includeLibraryLists: pending.includeLibraryLists,
  });
}

function applyCatalogInvalidation(
  queryClient: QueryClient,
  options: {
    itemIds: string[];
    libraryId?: number;
    allowDashboardRefetch: boolean;
    includeLibraryLists: boolean;
  },
) {
  const { itemIds, libraryId, allowDashboardRefetch, includeLibraryLists } = options;
  void invalidateMediaSurfaceQueries(queryClient, { itemIds, libraryId }).then(() => {
    bumpHomeRefreshSignal(queryClient);
  });
  if (includeLibraryLists) {
    void queryClient.invalidateQueries({
      queryKey: adminKeys.libraries(),
      refetchType: allowDashboardRefetch ? "active" : "none",
    });
    void queryClient.invalidateQueries({ queryKey: adminKeys.libraryMatchQueueStatuses() });
    void queryClient.invalidateQueries({ queryKey: libraryKeys.all });
  }
  void queryClient.invalidateQueries({
    queryKey: adminKeys.stats(),
    refetchType: allowDashboardRefetch ? "active" : "none",
  });
}
