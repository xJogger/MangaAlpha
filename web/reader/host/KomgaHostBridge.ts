/*
 * Copyright (C) Contributors to the Suwayomi project
 * Copyright (C) fake-komga-115 contributors
 *
 * This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at https://mozilla.org/MPL/2.0/.
 */

import { ApolloLink } from '@apollo/client';
import { from } from 'rxjs';

interface KomgaSeriesDTO {
    id: string;
    name: string;
    booksCount: number;
    booksUnreadCount?: number;
    metadata?: {
        title?: string;
        summary?: string;
        status?: string;
        readingDirection?: string;
        readingDirectionLock?: boolean;
        genres?: string[];
        tags?: string[];
        publisher?: string;
    };
    booksMetadata?: {
        authors?: { name: string; role: string }[];
    };
}

export interface SeriesAutoWebtoonPolicy {
    forceWebtoon: boolean;
    suppressAutoWebtoon: boolean;
}

export interface AutoWebtoonPageSample {
    pageIndex: number;
    naturalWidth: number;
    naturalHeight: number;
    totalPages: number;
}

export function shouldRecordAutoWebtoonSample(sample: AutoWebtoonPageSample): number | null {
    const { pageIndex, naturalWidth, naturalHeight, totalPages } = sample;
    if (pageIndex < 0 || pageIndex >= 5) return null;
    if (!Number.isFinite(naturalWidth) || !Number.isFinite(naturalHeight)) return null;
    if (naturalWidth < 200 || naturalHeight <= 0) return null;
    const ratio = naturalHeight / naturalWidth;
    if (ratio < 1) return null;
    if (pageIndex === 0 && totalPages >= 4 && ratio < 2.2) return null;
    return ratio;
}

export function isAutoWebtoonRatioTriggered(sampledRatios: ReadonlyMap<number, number>): boolean {
    let tallCount = 0;
    for (const ratio of sampledRatios.values()) {
        if (ratio >= 3.5) return true;
        if (ratio >= 2.5) {
            tallCount += 1;
            if (tallCount >= 2) return true;
        }
    }
    return false;
}

interface KomgaBookDTO {
    id: string;
    seriesId: string;
    seriesTitle?: string;
    name: string;
    number: number;
    fileLastModified?: string;
    media?: {
        pagesCount?: number;
    };
    metadata?: {
        title?: string;
        numberSort?: number;
    };
    readProgress?: {
        page?: number | null;
        completed?: boolean;
        readDate?: string | null;
    } | null;
}

interface ChapterRecord {
    chapterId: number;
    mangaId: number;
    seriesId: string;
    bookId: string;
    sourceOrder: number;
    chapterNumber: number;
    name: string;
    uploadDate: string;
    pageCount: number;
    isRead: boolean;
    lastPageRead: number;
    isBookmarked: boolean;
}

interface SeriesBundle {
    mangaId: number;
    seriesId: string;
    series: KomgaSeriesDTO;
    chaptersDesc: ChapterRecord[];
}

const LEGACY_SERIES_MAP_KEY = 'fk115_reader_series_map_v2';
const LEGACY_GLOBAL_META_KEY = 'fk115_reader_global_meta_v2';
const LEGACY_SERIES_META_PREFIX = 'fk115_reader_series_meta_v2:';

function readJSONStorageWithLegacy<T>(key: string, legacyKey: string | null, fallback: T): T {
    try {
        const raw = localStorage.getItem(key);
        if (raw) return JSON.parse(raw) as T;
        if (legacyKey) {
            const legacyRaw = localStorage.getItem(legacyKey);
            if (legacyRaw) {
                const parsed = JSON.parse(legacyRaw) as T;
                localStorage.setItem(key, legacyRaw);
                return parsed;
            }
        }
        return fallback;
    } catch {
        return fallback;
    }
}

function writeJSONStorage(key: string, value: unknown): void {
    try {
        localStorage.setItem(key, JSON.stringify(value));
    } catch {
        // Ignore quota errors
    }
}

export interface ArchivePasswordPromptRequest {
    bookId: string;
    bookName: string;
    seriesId: string;
    seriesTitle: string;
    invalid: boolean;
}

type ArchivePasswordPromptHandler = (req: ArchivePasswordPromptRequest) => Promise<boolean>;

interface KomgaAPIError extends Error {
    code?: string;
    status?: number;
}

async function komgaFetch<T>(path: string, init?: RequestInit): Promise<T> {
    const headers = new Headers(init?.headers ?? {});
    headers.set('X-FK115-UI', '1');
    if (init?.body !== undefined && !headers.has('Content-Type')) {
        headers.set('Content-Type', 'application/json');
    }
    const resp = await fetch(path, {
        ...init,
        credentials: 'same-origin',
        headers,
    });
    if (resp.status === 401) {
        const redirect = encodeURIComponent(window.location.pathname + window.location.search);
        window.location.assign(`/login?redirect=${redirect}`);
        throw new Error('Authentication required');
    }
    if (resp.status === 204) {
        return undefined as T;
    }
    if (!resp.ok) {
        let msg = `HTTP ${resp.status}`;
        let code: string | undefined;
        try {
            const errJson = await resp.json();
            if (errJson?.message) msg = errJson.message;
            if (typeof errJson?.error === 'string') code = errJson.error;
            else if (typeof errJson?.code === 'string') code = errJson.code;
        } catch {
            // ignore
        }
        const err = new Error(msg) as KomgaAPIError;
        err.code = code;
        err.status = resp.status;
        throw err;
    }
    return (await resp.json()) as T;
}

class KomgaBridgeImpl {
    private seriesToMangaId = new Map<string, number>();

    private mangaIdToSeries = new Map<number, string>();

    private nextMangaId = 1;

    private nextChapterId = 1;

    private bundles = new Map<number, SeriesBundle>();

    private chaptersById = new Map<number, ChapterRecord>();

    private bookKeyToChapterId = new Map<string, number>();

    private pendingProgressTimers = new Map<string, ReturnType<typeof setTimeout>>();

    private pendingProgressPayloads = new Map<string, { page: number; completed: boolean }>();

    private clientSettingsLoaded = false;

    private clientSettingsSyncPromise: Promise<void> | null = null;

    private currentUserId: string | null = null;

    private userContextPromise: Promise<string> | null = null;

    private seriesMapLoadedForUser: string | null = null;

    private passwordPromptHandler: ArchivePasswordPromptHandler | null = null;

    private inFlightBookPrompts = new Map<string, Promise<boolean>>();

    private passwordPromptQueue: Promise<void> = Promise.resolve();

    public setPasswordPromptHandler(handler: ArchivePasswordPromptHandler | null): void {
        this.passwordPromptHandler = handler;
    }

    private requestArchivePasswordUnlock(req: ArchivePasswordPromptRequest): Promise<boolean> {
        const existing = this.inFlightBookPrompts.get(req.bookId);
        if (existing) {
            return existing;
        }
        const task = new Promise<boolean>((resolve) => {
            this.passwordPromptQueue = this.passwordPromptQueue.then(async () => {
                const handler = this.passwordPromptHandler;
                if (!handler) {
                    resolve(false);
                    return;
                }
                try {
                    const unlocked = await handler(req);
                    resolve(unlocked);
                } catch {
                    resolve(false);
                }
            });
        }).finally(() => {
            if (this.inFlightBookPrompts.get(req.bookId) === task) {
                this.inFlightBookPrompts.delete(req.bookId);
            }
        });
        this.inFlightBookPrompts.set(req.bookId, task);
        return task;
    }

    public async unlockBookArchive(bookId: string, password: string): Promise<void> {
        await komgaFetch<{ ok: boolean; pageCount: number }>(
            `/admin/api/books/${encodeURIComponent(bookId)}/unlock`,
            {
                method: 'POST',
                body: JSON.stringify({ password }),
            },
        );
    }

    private seriesMapKey(): string {
        return this.currentUserId ? `${LEGACY_SERIES_MAP_KEY}:u:${this.currentUserId}` : '';
    }

    private globalMetaKey(): string {
        return this.currentUserId ? `${LEGACY_GLOBAL_META_KEY}:u:${this.currentUserId}` : '';
    }

    private seriesMetaKey(seriesId: string): string {
        return this.currentUserId ? `${LEGACY_SERIES_META_PREFIX}u:${this.currentUserId}:${seriesId}` : '';
    }

    private isPrimaryAdmin(): boolean {
        return this.currentUserId === 'admin';
    }

    private identityMismatch = false;

    private loadSeriesMapForCurrentUser(): void {
        const uid = this.currentUserId;
        if (!uid) return;
        if (this.seriesMapLoadedForUser === uid) return;
        this.seriesMapLoadedForUser = uid;
        this.seriesToMangaId.clear();
        this.mangaIdToSeries.clear();
        this.nextMangaId = 1;
        const saved = readJSONStorageWithLegacy<Record<string, number>>(
            this.seriesMapKey(),
            this.isPrimaryAdmin() ? LEGACY_SERIES_MAP_KEY : null,
            {},
        );
        Object.entries(saved).forEach(([sid, mid]) => {
            if (typeof mid === 'number' && mid > 0) {
                this.seriesToMangaId.set(sid, mid);
                this.mangaIdToSeries.set(mid, sid);
                if (mid >= this.nextMangaId) {
                    this.nextMangaId = mid + 1;
                }
            }
        });
    }

    private async ensureUserContext(): Promise<string> {
        if (this.currentUserId) {
            this.loadSeriesMapForCurrentUser();
            return this.currentUserId;
        }
        if (!this.userContextPromise) {
            this.userContextPromise = komgaFetch<{ id?: string }>('/api/v2/users/me')
                .then((me) => {
                    const uid = me?.id ? String(me.id).trim() : '';
                    if (!uid) {
                        this.userContextPromise = null;
                        return '';
                    }
                    this.currentUserId = uid;
                    this.loadSeriesMapForCurrentUser();
                    return uid;
                })
                .catch(() => {
                    // Do NOT fall back to 'admin' on transient lookup failure; allow retry.
                    this.userContextPromise = null;
                    return '';
                });
        }
        return this.userContextPromise;
    }

    private cancelAllPendingProgress(): void {
        this.pendingProgressTimers.forEach((t) => clearTimeout(t));
        this.pendingProgressTimers.clear();
        this.pendingProgressPayloads.clear();
    }

    private async revalidateUserContext(): Promise<void> {
        if (!this.currentUserId || this.identityMismatch) return;
        try {
            const me = await komgaFetch<{ id?: string }>('/api/v2/users/me');
            const uid = me?.id ? String(me.id).trim() : '';
            if (uid && uid !== this.currentUserId) {
                this.identityMismatch = true;
                this.cancelAllPendingProgress();
                window.location.reload();
            }
        } catch {
            // Session expired or transient network error; do not flush stale progress
        }
    }

    private userBoundHeaders(): Record<string, string> {
        return this.currentUserId ? { 'X-FK115-User-Id': this.currentUserId } : {};
    }

    constructor() {
        window.addEventListener('pagehide', () => this.flushAllProgress());
        window.addEventListener('pageshow', (e) => {
            if (e.persisted) {
                void this.revalidateUserContext();
            }
        });
        document.addEventListener('visibilitychange', () => {
            if (document.hidden) {
                this.flushAllProgress();
            } else {
                void this.revalidateUserContext();
            }
        });
    }

    private getOrCreateMangaId(seriesId: string): number {
        this.loadSeriesMapForCurrentUser();
        const existing = this.seriesToMangaId.get(seriesId);
        if (existing !== undefined) return existing;
        const assigned = this.nextMangaId++;
        this.seriesToMangaId.set(seriesId, assigned);
        this.mangaIdToSeries.set(assigned, seriesId);
        const mapKey = this.seriesMapKey();
        if (mapKey) {
            const obj: Record<string, number> = {};
            this.seriesToMangaId.forEach((v, k) => {
                obj[k] = v;
            });
            writeJSONStorage(mapKey, obj);
        }
        return assigned;
    }

    public getSeriesIdForManga(mangaId: number): string | undefined {
        return this.mangaIdToSeries.get(mangaId);
    }

    public getBookIdForChapter(chapterId: number): string | undefined {
        return this.chaptersById.get(chapterId)?.bookId;
    }

    public getSeriesAutoWebtoonPolicy(mangaId: number): SeriesAutoWebtoonPolicy {
        const series = this.bundles.get(mangaId)?.series;
        const dir = String(series?.metadata?.readingDirection ?? '').trim().toUpperCase();
        const locked = Boolean(series?.metadata?.readingDirectionLock);
        return {
            forceWebtoon: dir === 'VERTICAL' || dir === 'WEBTOON',
            suppressAutoWebtoon: locked && (dir === 'RIGHT_TO_LEFT' || dir === 'LEFT_TO_RIGHT'),
        };
    }

    public isAutoWebtoonDetected(mangaId: number): boolean {
        const seriesId = this.mangaIdToSeries.get(mangaId);
        if (!seriesId) return false;
        const storageKey = this.seriesMetaKey(seriesId);
        if (!storageKey) return false;
        const metaMap = readJSONStorageWithLegacy<Record<string, string>>(
            storageKey,
            this.isPrimaryAdmin() ? `${LEGACY_SERIES_META_PREFIX}${seriesId}` : null,
            {},
        );
        return metaMap.fk115_autoWebtoonDetected === '1';
    }

    public markAutoWebtoonDetected(mangaId: number): void {
        if (this.identityMismatch || !this.currentUserId) return;
        const seriesId = this.mangaIdToSeries.get(mangaId);
        if (!seriesId) return;
        const storageKey = this.seriesMetaKey(seriesId);
        if (!storageKey) return;
        const metaMap = readJSONStorageWithLegacy<Record<string, string>>(
            storageKey,
            this.isPrimaryAdmin() ? `${LEGACY_SERIES_META_PREFIX}${seriesId}` : null,
            {},
        );
        if (metaMap.fk115_autoWebtoonDetected === '1') return;
        metaMap.fk115_autoWebtoonDetected = '1';
        writeJSONStorage(storageKey, metaMap);
        this.syncClientSetting(`webui.reader.series.${seriesId}`, metaMap);
    }

    private async ensureClientSettingsSynced(): Promise<void> {
        const uid = await this.ensureUserContext();
        if (!uid) return;
        if (this.clientSettingsLoaded) return;
        if (!this.clientSettingsSyncPromise) {
            this.clientSettingsSyncPromise = (async () => {
                try {
                    const remote = await komgaFetch<Record<string, { value?: string }>>('/api/v1/client-settings/user/list');
                    const globalEntry = remote?.['webui.reader.globalMeta']?.value;
                    const key = this.globalMetaKey();
                    if (globalEntry && key) {
                        const parsed = JSON.parse(globalEntry) as Record<string, string>;
                        const local = readJSONStorageWithLegacy<Record<string, string>>(
                            key,
                            this.isPrimaryAdmin() ? LEGACY_GLOBAL_META_KEY : null,
                            {},
                        );
                        writeJSONStorage(key, { ...parsed, ...local });
                    }
                    Object.entries(remote ?? {}).forEach(([settingKey, entry]) => {
                        if (!settingKey.startsWith('webui.reader.series.') || !entry?.value) return;
                        const sid = settingKey.slice('webui.reader.series.'.length);
                        const sKey = sid ? this.seriesMetaKey(sid) : '';
                        if (!sKey) return;
                        try {
                            const parsed = JSON.parse(entry.value) as Record<string, string>;
                            const local = readJSONStorageWithLegacy<Record<string, string>>(
                                sKey,
                                this.isPrimaryAdmin() ? `${LEGACY_SERIES_META_PREFIX}${sid}` : null,
                                {},
                            );
                            writeJSONStorage(sKey, { ...parsed, ...local });
                        } catch {
                            // Ignore malformed series meta entry
                        }
                    });
                    this.clientSettingsLoaded = true;
                } catch {
                    // Non-fatal; allow future retry
                    this.clientSettingsSyncPromise = null;
                }
            })();
        }
        await this.clientSettingsSyncPromise;
    }

    private syncClientSetting(key: string, metaMap: Record<string, string>): void {
        if (!this.currentUserId || this.identityMismatch) return;
        const serialized = JSON.stringify(metaMap);
        if (serialized.length > 7800) return;
        komgaFetch('/api/v1/client-settings/user', {
            method: 'PATCH',
            headers: this.userBoundHeaders(),
            body: JSON.stringify({
                [key]: {
                    value: serialized,
                    allowUnauthenticated: false,
                },
            }),
        }).catch(() => {});
    }

    public async prepareEntry(
        seriesId: string,
        bookId: string,
        pageOneBased?: number,
    ): Promise<{ mangaId: number; sourceOrder: number }> {
        await this.ensureClientSettingsSynced();
        const bundle = await this.loadSeriesBundle(seriesId, true);
        const found = bundle.chaptersDesc.find((c) => c.bookId === bookId) ?? bundle.chaptersDesc[bundle.chaptersDesc.length - 1];
        if (!found) {
            throw new Error('该系列下没有可阅读的漫画卷');
        }
        if (pageOneBased !== undefined && pageOneBased >= 1) {
            found.lastPageRead = Math.max(0, pageOneBased - 1);
        }
        return { mangaId: bundle.mangaId, sourceOrder: found.sourceOrder };
    }

    private async loadSeriesBundle(seriesId: string, forceRefresh = false): Promise<SeriesBundle> {
        const mangaId = this.getOrCreateMangaId(seriesId);
        if (!forceRefresh) {
            const cached = this.bundles.get(mangaId);
            if (cached) return cached;
        }

        const [series, booksResp] = await Promise.all([
            komgaFetch<KomgaSeriesDTO>(`/api/v1/series/${encodeURIComponent(seriesId)}`),
            komgaFetch<{ content?: KomgaBookDTO[] }>(
                `/api/v1/series/${encodeURIComponent(seriesId)}/books?unpaged=true&sort=metadata.numberSort,asc`,
            ),
        ]);

        const booksAsc = booksResp.content ?? [];
        const chaptersAsc: ChapterRecord[] = booksAsc.map((b, idx) => {
            const sourceOrder = idx + 1;
            const bookKey = `${seriesId}::${b.id}`;
            let chapterId = this.bookKeyToChapterId.get(bookKey);
            if (chapterId === undefined) {
                chapterId = this.nextChapterId++;
                this.bookKeyToChapterId.set(bookKey, chapterId);
            }
            const rp = b.readProgress;
            const isRead = Boolean(rp?.completed);
            const lastPageRead = rp?.page && rp.page > 0 ? Math.max(0, rp.page - 1) : 0;
            const modifiedMs = b.fileLastModified ? Date.parse(b.fileLastModified) : Date.now();
            const rec: ChapterRecord = {
                chapterId,
                mangaId,
                seriesId,
                bookId: b.id,
                sourceOrder,
                chapterNumber: sourceOrder,
                name: b.metadata?.title || b.name,
                uploadDate: String(Number.isNaN(modifiedMs) ? Date.now() : modifiedMs),
                pageCount: b.media?.pagesCount ?? 0,
                isRead,
                lastPageRead,
                isBookmarked: false,
            };
            this.chaptersById.set(chapterId, rec);
            return rec;
        });

        const chaptersDesc = [...chaptersAsc].reverse();
        const bundle: SeriesBundle = {
            mangaId,
            seriesId,
            series,
            chaptersDesc,
        };
        this.bundles.set(mangaId, bundle);
        return bundle;
    }

    private async ensureBundleByMangaId(mangaId: number): Promise<SeriesBundle> {
        const existing = this.bundles.get(mangaId);
        if (existing) return existing;
        const seriesId = this.mangaIdToSeries.get(mangaId);
        if (!seriesId) {
            throw new Error('未找到对应的漫画系列上下文，请从书架重新打开');
        }
        return this.loadSeriesBundle(seriesId, false);
    }

    private scheduleProgressSync(bookId: string, pageOneBased: number, completed: boolean): void {
        if (this.identityMismatch || !this.currentUserId) return;
        const prevTimer = this.pendingProgressTimers.get(bookId);
        if (prevTimer) {
            clearTimeout(prevTimer);
            this.pendingProgressTimers.delete(bookId);
        }
        const payload = { page: Math.max(1, pageOneBased), completed };
        if (completed) {
            this.pendingProgressPayloads.delete(bookId);
            komgaFetch(`/api/v1/books/${encodeURIComponent(bookId)}/read-progress`, {
                method: 'PATCH',
                headers: this.userBoundHeaders(),
                body: JSON.stringify(payload),
                keepalive: true,
            }).catch(() => {});
            return;
        }
        this.pendingProgressPayloads.set(bookId, payload);
        const timer = setTimeout(() => {
            this.pendingProgressTimers.delete(bookId);
            if (this.identityMismatch || !this.currentUserId) return;
            const item = this.pendingProgressPayloads.get(bookId);
            if (!item) return;
            this.pendingProgressPayloads.delete(bookId);
            komgaFetch(`/api/v1/books/${encodeURIComponent(bookId)}/read-progress`, {
                method: 'PATCH',
                headers: this.userBoundHeaders(),
                body: JSON.stringify(item),
                keepalive: true,
            }).catch(() => {});
        }, 350);
        this.pendingProgressTimers.set(bookId, timer);
    }

    public flushAllProgress(): void {
        if (this.identityMismatch || !this.currentUserId) {
            this.cancelAllPendingProgress();
            return;
        }
        this.pendingProgressTimers.forEach((t) => clearTimeout(t));
        this.pendingProgressTimers.clear();
        this.pendingProgressPayloads.forEach((payload, bookId) => {
            fetch(`/api/v1/books/${encodeURIComponent(bookId)}/read-progress`, {
                method: 'PATCH',
                credentials: 'same-origin',
                keepalive: true,
                headers: {
                    'Content-Type': 'application/json',
                    'X-FK115-UI': '1',
                    ...this.userBoundHeaders(),
                },
                body: JSON.stringify(payload),
            }).catch(() => {});
        });
        this.pendingProgressPayloads.clear();
    }

    private buildMangaGql(bundle: SeriesBundle) {
        const s = bundle.series;
        const smKey = this.seriesMetaKey(bundle.seriesId);
        const metaMap = smKey
            ? readJSONStorageWithLegacy<Record<string, string>>(
                  smKey,
                  this.isPrimaryAdmin() ? `${LEGACY_SERIES_META_PREFIX}${bundle.seriesId}` : null,
                  {},
              )
            : {};
        const metaNodes = Object.entries(metaMap).map(([key, value]) => ({
            __typename: 'MangaMetaType',
            mangaId: bundle.mangaId,
            key,
            value,
        }));
        const authors = (s.booksMetadata?.authors ?? []).map((a) => a.name).join(', ');
        const unreadCount = bundle.chaptersDesc.filter((c) => !c.isRead).length;
        const genreSet = new Set<string>();
        [...(s.metadata?.genres ?? []), ...(s.metadata?.tags ?? [])].forEach((tag) => {
            const trimmed = typeof tag === 'string' ? tag.trim() : '';
            if (trimmed) genreSet.add(trimmed);
        });
        const mergedGenres = Array.from(genreSet);
        return {
            __typename: 'MangaType',
            id: bundle.mangaId,
            title: s.metadata?.title || s.name,
            thumbnailUrl: `/api/v1/series/${encodeURIComponent(bundle.seriesId)}/thumbnail`,
            thumbnailUrlLastFetched: 0,
            inLibrary: true,
            initialized: true,
            sourceId: 'komga-local',
            genre: mergedGenres,
            status: s.metadata?.status || 'ONGOING',
            author: authors,
            artist: authors,
            description: s.metadata?.summary ?? '',
            unreadCount,
            downloadCount: bundle.chaptersDesc.length,
            bookmarkCount: 0,
            hasDuplicateChapters: false,
            lastFetchedAt: '0',
            inLibraryAt: '0',
            firstUnreadChapter: null,
            lastReadChapter: null,
            latestReadChapter: null,
            latestFetchedChapter: null,
            latestUploadedChapter: null,
            highestNumberedChapter: null,
            source: {
                __typename: 'SourceType',
                id: 'komga-local',
                name: 'fake-komga-115',
                displayName: 'MangaAlpha',
                lang: 'zh',
                iconUrl: '',
            },
            meta: metaNodes,
            chapters: {
                __typename: 'ChapterNodeList',
                totalCount: bundle.chaptersDesc.length,
            },
            trackRecords: {
                __typename: 'TrackRecordNodeList',
                totalCount: 0,
                nodes: [],
            },
        };
    }

    private buildChapterGql(c: ChapterRecord) {
        return {
            __typename: 'ChapterType',
            id: c.chapterId,
            name: c.name,
            mangaId: c.mangaId,
            scanlator: '',
            realUrl: `/series/${encodeURIComponent(c.seriesId)}`,
            sourceOrder: c.sourceOrder,
            chapterNumber: c.chapterNumber,
            isRead: c.isRead,
            isDownloaded: true,
            isBookmarked: c.isBookmarked,
            uploadDate: c.uploadDate,
            fetchedAt: c.uploadDate,
            lastReadAt: '0',
            lastPageRead: c.lastPageRead,
            pageCount: c.pageCount,
        };
    }

    private async executeOperation(operationName: string, variables: Record<string, any>): Promise<Record<string, any>> {
        await this.ensureUserContext();
        switch (operationName) {
            case 'GET_GLOBAL_METADATAS': {
                await this.ensureClientSettingsSynced();
                const gKey = this.globalMetaKey();
                const globalMap = gKey
                    ? readJSONStorageWithLegacy<Record<string, string>>(
                          gKey,
                          this.isPrimaryAdmin() ? LEGACY_GLOBAL_META_KEY : null,
                          {},
                      )
                    : {};
                const nodes = Object.entries(globalMap).map(([key, value]) => ({
                    __typename: 'GlobalMetaType',
                    key,
                    value,
                }));
                return {
                    metas: {
                        __typename: 'GlobalMetaNodeList',
                        nodes,
                        pageInfo: {
                            __typename: 'PageInfo',
                            endCursor: null,
                            hasNextPage: false,
                            hasPreviousPage: false,
                            startCursor: null,
                        },
                        totalCount: nodes.length,
                    },
                };
            }

            case 'GET_GLOBAL_METADATA': {
                const gKey = this.globalMetaKey();
                const globalMap = gKey
                    ? readJSONStorageWithLegacy<Record<string, string>>(
                          gKey,
                          this.isPrimaryAdmin() ? LEGACY_GLOBAL_META_KEY : null,
                          {},
                      )
                    : {};
                const key = String(variables?.key ?? '');
                return {
                    meta:
                        key in globalMap
                            ? { __typename: 'GlobalMetaType', key, value: globalMap[key] }
                            : null,
                };
            }

            case 'UPDATE_GLOBAL_METADATA': {
                const gKey = this.globalMetaKey();
                const globalMap = gKey
                    ? readJSONStorageWithLegacy<Record<string, string>>(
                          gKey,
                          this.isPrimaryAdmin() ? LEGACY_GLOBAL_META_KEY : null,
                          {},
                      )
                    : {};
                const applyDeletes = (keys?: string[]) => {
                    (keys ?? []).forEach((k) => {
                        delete globalMap[k];
                    });
                };
                const applySets = (metas?: { key: string; value: string }[]) => {
                    (metas ?? []).forEach((m) => {
                        globalMap[m.key] = m.value;
                    });
                };
                if (variables?.hasPreUpdateDeletions) applyDeletes(variables.preUpdateDeleteInput?.keys);
                if (variables?.hasUpdates) applySets(variables.updateInput?.metas);
                if (variables?.hasPostUpdateDeletions) applyDeletes(variables.postUpdateDeleteInput?.keys);
                if (variables?.isMigration) applySets(variables.migrateInput?.metas);
                if (gKey) {
                    writeJSONStorage(gKey, globalMap);
                    this.syncClientSetting('webui.reader.globalMeta', globalMap);
                }

                const nodes = Object.entries(globalMap).map(([key, value]) => ({
                    __typename: 'GlobalMetaType',
                    key,
                    value,
                }));
                return {
                    ...(variables?.hasPreUpdateDeletions ? { preUpdateDeletedMeta: { __typename: 'DeleteGlobalMetasPayload', metas: nodes } } : {}),
                    ...(variables?.hasUpdates ? { updatedMeta: { __typename: 'SetGlobalMetasPayload', metas: nodes } } : {}),
                    ...(variables?.hasPostUpdateDeletions ? { postUpdateDeletedMeta: { __typename: 'DeleteGlobalMetasPayload', metas: nodes } } : {}),
                    ...(variables?.isMigration ? { migrationMeta: { __typename: 'SetGlobalMetasPayload', metas: nodes } } : {}),
                };
            }

            case 'GET_SERVER_SETTINGS': {
                return {
                    settings: {
                        __typename: 'SettingsType',
                        ip: '127.0.0.1',
                        port: 25600,
                        socksProxyEnabled: false,
                        socksProxyVersion: 5,
                        socksProxyHost: '',
                        socksProxyPort: '',
                        socksProxyUsername: '',
                        socksProxyPassword: '',
                        webUIFlavor: 'WebUI',
                        initialOpenInBrowserEnabled: false,
                        webUIInterface: 'BROWSER',
                        electronPath: '',
                        webUIChannel: 'STABLE',
                        webUIUpdateCheckInterval: 0,
                        downloadAsCbz: false,
                        downloadsPath: '',
                        autoDownloadNewChapters: false,
                        excludeEntryWithUnreadChapters: false,
                        autoDownloadNewChaptersLimit: 0,
                        autoDownloadIgnoreReUploads: false,
                        downloadConversions: [],
                        serveConversions: [],
                        maxSourcesInParallel: 1,
                        excludeUnreadChapters: false,
                        excludeNotStarted: false,
                        excludeCompleted: false,
                        globalUpdateInterval: 0,
                        updateMangas: false,
                        authMode: 'NONE',
                        authPassword: '',
                        authUsername: '',
                        jwtAudience: '',
                        jwtTokenExpiry: '0',
                        jwtRefreshExpiry: '0',
                        debugLogsEnabled: false,
                        systemTrayEnabled: false,
                        maxLogFileSize: '',
                        maxLogFiles: 0,
                        maxLogFolderSize: '',
                        backupPath: '',
                        backupTime: '',
                        backupInterval: 0,
                        backupTTL: 0,
                        autoBackupIncludeCategories: false,
                        autoBackupIncludeChapters: false,
                        autoBackupIncludeClientData: false,
                        autoBackupIncludeHistory: false,
                        autoBackupIncludeManga: false,
                        autoBackupIncludeServerSettings: false,
                        autoBackupIncludeTracking: false,
                        localSourcePath: '',
                        flareSolverrEnabled: false,
                        flareSolverrUrl: '',
                        flareSolverrTimeout: 0,
                        flareSolverrSessionName: '',
                        flareSolverrSessionTtl: 0,
                        flareSolverrAsResponseFallback: false,
                        opdsUseBinaryFileSizes: false,
                        opdsItemsPerPage: 50,
                        opdsEnablePageReadProgress: true,
                        opdsMarkAsReadOnDownload: false,
                        opdsShowOnlyUnreadChapters: false,
                        opdsShowOnlyDownloadedChapters: false,
                        opdsChapterSortOrder: 'ASC',
                        opdsCbzMimetype: 'ONE',
                        opdsSkipChapterMetadataFeed: false,
                        koreaderSyncChecksumMethod: 'BINARY',
                        koreaderSyncStrategyBackward: 'DISABLED',
                        koreaderSyncStrategyForward: 'DISABLED',
                        koreaderSyncPercentageTolerance: 0,
                        databaseType: 'H2',
                        databaseUrl: '',
                        databaseUsername: '',
                        databasePassword: '',
                        databaseSchema: '',
                        useHikariConnectionPool: false,
                        hikariMaxPoolSize: 0,
                        kcefEnabled: false,
                        syncDataCategories: false,
                        syncDataChapters: false,
                        syncDataHistory: false,
                        syncDataManga: false,
                        syncDataTracking: false,
                        syncInterval: 0,
                        syncYomiApiKey: '',
                        syncYomiEnabled: false,
                        syncYomiHost: '',
                    },
                };
            }

            case 'GET_MANGA_READER':
            case 'GET_MANGA_SCREEN':
            case 'GET_MANGA_META': {
                await this.ensureClientSettingsSynced();
                const mangaId = Number(variables?.id);
                const bundle = await this.ensureBundleByMangaId(mangaId);
                return {
                    manga: this.buildMangaGql(bundle),
                };
            }

            case 'UPDATE_MANGA_METADATA': {
                const updatedByManga = new Map<number, Record<string, string>>();
                const getMap = (mangaId: number): Record<string, string> => {
                    let map = updatedByManga.get(mangaId);
                    if (map) return map;
                    const seriesId = this.mangaIdToSeries.get(mangaId) ?? String(mangaId);
                    const storageKey = this.seriesMetaKey(seriesId);
                    map = storageKey
                        ? readJSONStorageWithLegacy<Record<string, string>>(
                              storageKey,
                              this.isPrimaryAdmin() ? `${LEGACY_SERIES_META_PREFIX}${seriesId}` : null,
                              {},
                          )
                        : {};
                    updatedByManga.set(mangaId, map);
                    return map;
                };
                const applyDeletes = (mangaId: number, keys?: string[]) => {
                    const map = getMap(mangaId);
                    (keys ?? []).forEach((k) => {
                        delete map[k];
                    });
                };
                const applySets = (mangaId: number, metas?: { key: string; value: string }[]) => {
                    const map = getMap(mangaId);
                    (metas ?? []).forEach((m) => {
                        map[m.key] = m.value;
                    });
                };
                const collectNodes = (mangaId: number) => {
                    const map = getMap(mangaId);
                    return Object.entries(map).map(([key, value]) => ({
                        __typename: 'MangaMetaType',
                        mangaId,
                        key,
                        value,
                    }));
                };

                const handleDeleteInput = (input: any): any[] => {
                    const nodes: any[] = [];
                    if (Array.isArray(input?.items)) {
                        input.items.forEach((item: { mangaIds?: number[]; keys?: string[] }) => {
                            (item.mangaIds ?? []).forEach((mid) => {
                                const id = Number(mid);
                                applyDeletes(id, item.keys);
                                nodes.push(...collectNodes(id));
                            });
                        });
                    } else if (input?.mangaId !== undefined) {
                        const id = Number(input.mangaId);
                        applyDeletes(id, input.keys);
                        nodes.push(...collectNodes(id));
                    }
                    return nodes;
                };

                const handleSetInput = (input: any): any[] => {
                    const nodes: any[] = [];
                    if (Array.isArray(input?.items)) {
                        input.items.forEach((item: { mangaIds?: number[]; metas?: { key: string; value: string }[] }) => {
                            (item.mangaIds ?? []).forEach((mid) => {
                                const id = Number(mid);
                                applySets(id, item.metas);
                                nodes.push(...collectNodes(id));
                            });
                        });
                    } else if (Array.isArray(input?.metas) && input.metas.length > 0) {
                        const id = Number(input.metas[0].mangaId);
                        applySets(id, input.metas);
                        nodes.push(...collectNodes(id));
                    }
                    return nodes;
                };

                const preDeletedNodes = variables?.hasPreUpdateDeletions ? handleDeleteInput(variables.preUpdateDeleteInput) : [];
                const updatedNodes = variables?.hasUpdates ? handleSetInput(variables.updateInput) : [];
                const postDeletedNodes = variables?.hasPostUpdateDeletions ? handleDeleteInput(variables.postUpdateDeleteInput) : [];
                const migrationNodes = variables?.isMigration ? handleSetInput(variables.migrateInput) : [];

                updatedByManga.forEach((metaMap, mangaId) => {
                    const seriesId = this.mangaIdToSeries.get(mangaId) ?? String(mangaId);
                    const storageKey = this.seriesMetaKey(seriesId);
                    if (storageKey) {
                        writeJSONStorage(storageKey, metaMap);
                        this.syncClientSetting(`webui.reader.series.${seriesId}`, metaMap);
                    }
                });

                return {
                    ...(variables?.hasPreUpdateDeletions ? { preUpdateDeletedMeta: { __typename: 'DeleteMangaMetasPayload', metas: preDeletedNodes } } : {}),
                    ...(variables?.hasUpdates ? { updatedMeta: { __typename: 'SetMangaMetasPayload', metas: updatedNodes } } : {}),
                    ...(variables?.hasPostUpdateDeletions ? { postUpdateDeletedMeta: { __typename: 'DeleteMangaMetasPayload', metas: postDeletedNodes } } : {}),
                    ...(variables?.isMigration ? { migrationMeta: { __typename: 'SetMangaMetasPayload', metas: migrationNodes } } : {}),
                };
            }

            case 'GET_CHAPTERS_READER':
            case 'GET_CHAPTERS_MANGA': {
                const mangaId = Number(variables?.condition?.mangaId ?? variables?.mangaId);
                const bundle = await this.ensureBundleByMangaId(mangaId);
                const nodes = bundle.chaptersDesc.map((c) => this.buildChapterGql(c));
                return {
                    chapters: {
                        __typename: 'ChapterNodeList',
                        nodes,
                        pageInfo: {
                            __typename: 'PageInfo',
                            endCursor: null,
                            hasNextPage: false,
                            hasPreviousPage: false,
                            startCursor: null,
                        },
                        totalCount: nodes.length,
                    },
                };
            }

            case 'GET_CHAPTER_PAGES_FETCH': {
                const chapterId = Number(variables?.input?.chapterId);
                const chap = this.chaptersById.get(chapterId);
                if (!chap) {
                    throw new Error(`未找到章节缓存 (#${chapterId})`);
                }
                const bundle = this.bundles.get(chap.mangaId);
                let komgaPages: { number: number }[] | null = null;
                while (komgaPages === null) {
                    try {
                        komgaPages = await komgaFetch<{ number: number }[]>(
                            `/api/v1/books/${encodeURIComponent(chap.bookId)}/pages`,
                        );
                    } catch (err) {
                        const apiErr = err as KomgaAPIError;
                        const isPwErr =
                            apiErr?.code === 'ARCHIVE_PASSWORD_REQUIRED' ||
                            apiErr?.code === 'ARCHIVE_PASSWORD_INVALID';
                        if (!isPwErr || !this.passwordPromptHandler) {
                            throw err;
                        }
                        const seriesTitle =
                            bundle?.series.metadata?.title?.trim() || bundle?.series.name || '';
                        // If another chapter's password dialog is currently open, wait for it first
                        // and retry /pages in case same-folder auto-retry already unlocked this book.
                        if (this.inFlightBookPrompts.size > 0 && !this.inFlightBookPrompts.has(chap.bookId)) {
                            await this.passwordPromptQueue;
                            continue;
                        }
                        const unlocked = await this.requestArchivePasswordUnlock({
                            bookId: chap.bookId,
                            bookName: chap.name,
                            seriesId: chap.seriesId,
                            seriesTitle,
                            invalid: apiErr.code === 'ARCHIVE_PASSWORD_INVALID',
                        });
                        if (!unlocked) {
                            throw new Error('已取消输入压缩包密码');
                        }
                    }
                }
                const count = komgaPages.length;
                chap.pageCount = count;
                const pages = komgaPages.map(
                    (p) => `/api/v1/books/${encodeURIComponent(chap.bookId)}/pages/${p.number}`,
                );
                return {
                    fetchChapterPages: {
                        __typename: 'FetchChapterPagesPayload',
                        chapter: {
                            __typename: 'ChapterType',
                            id: chap.chapterId,
                            pageCount: count,
                            isDownloaded: true,
                            manga: {
                                __typename: 'MangaType',
                                id: chap.mangaId,
                                downloadCount: bundle?.chaptersDesc.length ?? 1,
                            },
                        },
                        pages,
                    },
                };
            }

            case 'UPDATE_CHAPTER':
            case 'UPDATE_CHAPTERS': {
                const isPlural = operationName === 'UPDATE_CHAPTERS';
                const ids: number[] = isPlural
                    ? (variables?.input?.ids ?? []).map(Number)
                    : [Number(variables?.input?.id)];
                const patch = variables?.input?.patch ?? {};
                const updatedChapters = ids
                    .map((id) => this.chaptersById.get(id))
                    .filter((c): c is ChapterRecord => Boolean(c))
                    .map((c) => {
                        if (typeof patch.lastPageRead === 'number') {
                            c.lastPageRead = Math.max(0, patch.lastPageRead);
                        }
                        if (typeof patch.isRead === 'boolean') {
                            c.isRead = patch.isRead;
                        }
                        if (typeof patch.isBookmarked === 'boolean') {
                            c.isBookmarked = patch.isBookmarked;
                        }

                        if (patch.isRead === true) {
                            const finalPage = c.pageCount > 0 ? c.pageCount : c.lastPageRead + 1;
                            this.scheduleProgressSync(c.bookId, finalPage, true);
                        } else if (patch.isRead === false && patch.lastPageRead === undefined) {
                            if (!this.identityMismatch && this.currentUserId) {
                                komgaFetch(`/api/v1/books/${encodeURIComponent(c.bookId)}/read-progress`, {
                                    method: 'DELETE',
                                    headers: this.userBoundHeaders(),
                                }).catch(() => {});
                            }
                        } else if (typeof patch.lastPageRead === 'number') {
                            this.scheduleProgressSync(c.bookId, c.lastPageRead + 1, c.isRead);
                        }

                        const bundle = this.bundles.get(c.mangaId);
                        const unreadCount = bundle ? bundle.chaptersDesc.filter((x) => !x.isRead).length : 0;
                        return {
                            __typename: 'ChapterType',
                            id: c.chapterId,
                            isBookmarked: c.isBookmarked,
                            isRead: c.isRead,
                            lastReadAt: String(Math.floor(Date.now() / 1000)),
                            lastPageRead: c.lastPageRead,
                            manga: {
                                __typename: 'MangaType',
                                id: c.mangaId,
                                unreadCount,
                                bookmarkCount: 0,
                                lastReadChapter: { __typename: 'ChapterType', id: c.chapterId },
                                latestReadChapter: { __typename: 'ChapterType', id: c.chapterId },
                                firstUnreadChapter: null,
                            },
                        };
                    });

                if (isPlural) {
                    return {
                        updateChapters: {
                            __typename: 'UpdateChaptersPayload',
                            chapters: updatedChapters,
                        },
                    };
                }
                return {
                    updateChapter: {
                        __typename: 'UpdateChapterPayload',
                        chapter: updatedChapters[0] ?? null,
                    },
                };
            }

            default:
                return {};
        }
    }

    public createApolloLink(): ApolloLink {
        return new ApolloLink((operation) =>
            from(
                this.executeOperation(operation.operationName ?? '', operation.variables ?? {}).then((data) => ({
                    data,
                })),
            ),
        );
    }
}

export const KomgaHostBridge = new KomgaBridgeImpl();
