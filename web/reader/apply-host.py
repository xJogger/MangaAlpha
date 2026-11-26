#!/usr/bin/env python3
"""
Deterministic host adapter patcher for upstream Suwayomi-WebUI (MPL-2.0).
Adapts the Reader module to run standalone under /reader/* backed by Komga v1 REST APIs.
"""

from __future__ import annotations

import json
import shutil
import sys
from pathlib import Path


def replace_exact(path: Path, old: str, new: str) -> None:
    text = path.read_text(encoding="utf-8")
    if old not in text:
        raise RuntimeError(f"Expected snippet not found in {path}:\n{old[:180]}")
    path.write_text(text.replace(old, new, 1), encoding="utf-8")


def main() -> None:
    if len(sys.argv) != 3:
        raise SystemExit("Usage: apply-host.py <host_dir> <suwayomi_src_root>")

    host_dir = Path(sys.argv[1]).resolve()
    root = Path(sys.argv[2]).resolve()

    # 1. Copy host adapter modules
    target_host_dir = root / "src" / "host"
    target_host_dir.mkdir(parents=True, exist_ok=True)
    shutil.copy2(host_dir / "host" / "KomgaHostBridge.ts", target_host_dir / "KomgaHostBridge.ts")
    shutil.copy2(host_dir / "host" / "StreamingLoadStatus.tsx", target_host_dir / "StreamingLoadStatus.tsx")
    shutil.copy2(host_dir / "host" / "KomgaReaderApp.tsx", root / "src" / "App.tsx")

    # 2. Relax Node engine constraint & simplify build script in package.json
    pkg_path = root / "package.json"
    pkg = json.loads(pkg_path.read_text(encoding="utf-8"))
    pkg.pop("engines", None)
    pkg["scripts"]["build"] = "tsx tools/scripts/setupEnvFiles.ts && tsc --noEmit && vite build"
    pkg["scripts"]["prepare"] = "node -e ''"
    pkg_path.write_text(json.dumps(pkg, indent=2) + "\n", encoding="utf-8")

    # 3. Configure Vite for /reader/ subpath without ServiceWorker (PWA)
    vite_cfg = root / "vite.config.ts"
    vite_cfg.write_text(
        """/*
 * Copyright (C) Contributors to the Suwayomi project
 *
 * This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at https://mozilla.org/MPL/2.0/.
 */

import path from 'path';
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react-swc';
import { lingui } from '@lingui/vite-plugin';
import 'dotenv/config';

export default defineConfig(() => ({
    base: '/reader/',
    build: {
        outDir: 'build',
        sourcemap: false,
    },
    resolve: {
        tsconfigPaths: true,
        alias: {
            '@': path.resolve(import.meta.dirname, './src'),
        },
    },
    optimizeDeps: {
        include: ['@mui/material/Tooltip'],
    },
    plugins: [
        react({
            plugins: [['@lingui/swc-plugin', {}]],
        }),
        lingui({
            failOnCompileError: true,
        }),
    ],
}));
""",
        encoding="utf-8",
    )

    # 4. Clean index.html title, lang="zh-CN", & dark background
    idx_html = root / "index.html"
    html_text = idx_html.read_text(encoding="utf-8")
    html_text = html_text.replace('<html lang="en">', '<html lang="zh-CN">')
    html_text = html_text.replace("<title>Suwayomi</title>", "<title>阅读器 · MangaAlpha</title>")
    ui_dir = host_dir.parent.parent / "internal" / "httpserver" / "static" / "ui"
    shutil.copyfile(ui_dir / "mangaalpha-icon.png", root / "public" / "mangaalpha-icon.png")
    shutil.copyfile(ui_dir / "mangaalpha-icon-dark.png", root / "public" / "mangaalpha-icon-dark.png")
    upstream_tab_icons = (
        '<link rel="icon" type="image/png" href="/favicon-96x96.png" sizes="96x96" />\n'
        '        <link rel="icon" type="image/svg+xml" href="/favicon.svg" />\n'
        '        <link rel="shortcut icon" href="/favicon.ico" />'
    )
    if upstream_tab_icons not in html_text:
        raise RuntimeError(f"Expected tab icons not found in {idx_html}")
    favicon_snippet = (
        '<link id="app-favicon" rel="icon" type="image/png" href="/mangaalpha-icon.png" />\n'
        '        <script>(function(){var mq=window.matchMedia&&window.matchMedia("(prefers-color-scheme: dark)");'
        'function sync(){try{var m=localStorage.getItem("fk115_theme_mode")||"auto";'
        'var dark=(m==="dark")||(m!=="light"&&mq&&mq.matches);'
        'var l=document.getElementById("app-favicon");'
        'if(l)l.href=dark?"/reader/mangaalpha-icon-dark.png":"/reader/mangaalpha-icon.png";}catch(e){}}'
        'sync();if(mq&&mq.addEventListener)mq.addEventListener("change",sync);'
        'window.addEventListener("storage",function(ev){if(!ev.key||ev.key==="fk115_theme_mode")sync();});})();</script>'
    )
    html_text = html_text.replace(upstream_tab_icons, favicon_snippet, 1)
    if "<style>" not in html_text:
        html_text = html_text.replace(
            "</head>",
            "<style>html,body,#root{background:#000;margin:0;padding:0;}</style></head>",
        )
    idx_html.write_text(html_text, encoding="utf-8")

    # 4b. Default localization to Simplified Chinese (zh-Hans) and await catalog before initial render
    i18n_ts = root / "src" / "i18n" / "index.ts"
    replace_exact(
        i18n_ts,
        "const DEFAULT_LOCALE = 'en';",
        "const DEFAULT_LOCALE = 'zh-Hans';",
    )

    iso_lang_ts = root / "src" / "lib" / "ISOLanguageUtil.ts"
    replace_exact(
        iso_lang_ts,
        "export const DEFAULT_LANGUAGE = 'en';\n\nexport const SELECTED_LANGUAGE_CODE_KEY = 'i18n:selected-language';",
        "export const DEFAULT_LANGUAGE = 'zh-Hans';\n\nexport const SELECTED_LANGUAGE_CODE_KEY = 'fk115:reader:language';",
    )
    replace_exact(
        iso_lang_ts,
        """export function detectLocale(): string {
    const storedLanguage = AppStorage.local.getItem(SELECTED_LANGUAGE_CODE_KEY);
    const preferredLanguages = getPreferredISOLanguageCodes();
    const languageCodes = [storedLanguage, ...preferredLanguages];

    const normalizedLanguageCodes = languageCodes
        .filter((code) => code !== null)
        .filter((code) => getISOLanguage(code))
        .filter((code) => i18nResources.includes(code as (typeof i18nResources)[number]));

    return normalizedLanguageCodes[0] ?? DEFAULT_LANGUAGE;
}""",
        """export function detectLocale(): string {
    const storedLanguage = AppStorage.local.getItem(SELECTED_LANGUAGE_CODE_KEY);
    if (storedLanguage && i18nResources.includes(storedLanguage as (typeof i18nResources)[number])) {
        return storedLanguage;
    }
    return DEFAULT_LANGUAGE;
}""",
    )

    locale_importer_ts = root / "src" / "lib" / "dayjs" / "LocaleImporter.ts"
    replace_exact(
        locale_importer_ts,
        "import { DEFAULT_LANGUAGE } from '@/lib/ISOLanguageUtil.ts';\n",
        "",
    )
    replace_exact(
        locale_importer_ts,
        "    return DEFAULT_LANGUAGE;",
        "    return 'zh-cn';",
    )

    entry_tsx = root / "src" / "index.tsx"
    replace_exact(
        entry_tsx,
        """initializeLocalization().catch(defaultPromiseErrorHandler('i18n', true));

const container = document.getElementById('root');
const root = createRoot(container!);
root.render(
    <StrictMode>
        <App />
    </StrictMode>,
);""",
        """const container = document.getElementById('root');
const root = createRoot(container!);
initializeLocalization()
    .catch(defaultPromiseErrorHandler('i18n', true))
    .finally(() => {
        root.render(
            <StrictMode>
                <App />
            </StrictMode>,
        );
    });""",
    )

    # 5. SubpathUtil: Router basename = '/reader', API base URL = root origin (no '/reader' suffix)
    subpath_ts = root / "src" / "lib" / "utils" / "SubpathUtil.ts"
    subpath_ts.write_text(
        """/*
 * Copyright (C) Contributors to the Suwayomi project
 *
 * This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at https://mozilla.org/MPL/2.0/.
 */

export class SubpathUtil {
    public static getSubpath(): string {
        return '/reader';
    }

    public static getRouterBasename(): string | undefined {
        return '/reader';
    }

    public static maybeAddAsSuffix(serverUrl: string): string {
        return serverUrl;
    }

    public static getPathname(): string {
        const p = window.location.pathname;
        return p.startsWith('/reader') ? p.slice('/reader'.length) || '/' : p;
    }
}
""",
        encoding="utf-8",
    )

    # 6. AuthManager: session cookie auth is handled by fake-komga-115 HTTP middleware
    auth_ts = root / "src" / "features" / "authentication" / "AuthManager.ts"
    replace_exact(
        auth_ts,
        "    private static authInitialized: boolean = false;\n\n    private static authRequired: boolean | null = null;",
        "    private static authInitialized: boolean = true;\n\n    private static authRequired: boolean | null = false;",
    )

    # 7. GraphQLClient: route all Apollo operations through in-process KomgaHostBridge link (zero WebSocket)
    gql_client = root / "src" / "lib" / "requests" / "client" / "GraphQLClient.ts"
    replace_exact(
        gql_client,
        "import { BaseClient } from '@/lib/requests/client/BaseClient.ts';",
        "import { BaseClient } from '@/lib/requests/client/BaseClient.ts';\nimport { KomgaHostBridge } from '@/host/KomgaHostBridge.ts';",
    )
    replace_exact(
        gql_client,
        """    override reset(): void {
        super.reset();

        this.client.clearStore();
        this.client.stop();

        this.resetWsClient(false);
        this.createClient();
    }""",
        """    override reset(): void {
        super.reset();

        this.client.clearStore();
        this.client.stop();

        this.createClient();
    }""",
    )
    replace_exact(
        gql_client,
        """    protected createClient(createWsClientLazily?: boolean) {
        this.createWSClient(createWsClientLazily);
        this.client = new ApolloClient({
            cache: new InMemoryCache({ typePolicies }),
            devtools: { enabled: true },
            link: this.createLink(),
        });
    }""",
        """    protected createClient(_createWsClientLazily?: boolean) {
        this.client = new ApolloClient({
            cache: new InMemoryCache({ typePolicies }),
            devtools: { enabled: false },
            link: KomgaHostBridge.createApolloLink(),
        });
    }""",
    )

    # 8. Manga-friendly reader defaults (RTL, Double-page on wide screens, Single-page on mobile, Black canvas)
    reader_consts = root / "src" / "features" / "reader" / "settings" / "ReaderSettings.constants.tsx"
    replace_exact(
        reader_consts,
        """    pageScaleMode: ReaderPageScaleMode.ORIGINAL,
    shouldStretchPage: false,
    shouldOffsetDoubleSpreads: false,
    shouldSkipDupChapters: true,
    shouldSkipFilteredChapters: false,
    shouldShowPageNumber: true,
    isStaticNav: false,
    readingDirection: ReadingDirection.LTR,
    readingMode: ReadingMode.SINGLE_PAGE,
    exitMode: ReaderExitMode.PREVIOUS,
    backgroundColor: ReaderBackgroundColor.THEME,""",
        """    pageScaleMode:
        typeof window !== 'undefined' && window.innerWidth >= 900
            ? ReaderPageScaleMode.SCREEN
            : ReaderPageScaleMode.WIDTH,
    shouldStretchPage: false,
    shouldOffsetDoubleSpreads: false,
    shouldSkipDupChapters: true,
    shouldSkipFilteredChapters: false,
    shouldShowPageNumber: true,
    isStaticNav: false,
    readingDirection: ReadingDirection.RTL,
    readingMode:
        typeof window !== 'undefined' && window.innerWidth >= 900
            ? ReadingMode.DOUBLE_PAGE
            : ReadingMode.SINGLE_PAGE,
    exitMode: ReaderExitMode.MANGA,
    backgroundColor: ReaderBackgroundColor.BLACK,""",
    )
    replace_exact(
        reader_consts,
        """    shouldShowReadingModePreview: true,
    shouldShowTapZoneLayoutPreview: true,""",
        """    shouldShowReadingModePreview: false,
    shouldShowTapZoneLayoutPreview: false,""",
    )
    replace_exact(
        reader_consts,
        "    shouldCropBorders: false,",
        "    shouldCropBorders: typeof window !== 'undefined' && window.innerWidth < 900,",
    )

    # 8a0. Narrow tag table so generic Manhwa/Manhua/韩漫/国漫/만화/漫画 tags don't trigger webtoon mode
    manga_consts = root / "src" / "features" / "manga" / "Manga.constants.ts"
    replace_exact(
        manga_consts,
        """export const MANGA_TAGS_BY_MANGA_TYPE: Record<MangaType, string[]> = {
    [MangaType.MANGA]: ['Manga', 'מנגה', '만화', '漫画', 'مانجا', 'مانگا', 'Mangá', 'Манга', 'மங்கா-', 'Truyện Nhật'],
    [MangaType.COMIC]: ['Còmic', 'Comic', 'BD', 'קומיקס', 'Komik', '코믹', 'Quadrinho', '美漫'],
    [MangaType.WEBTOON]: [
        'Webtoon',
        'וובטון',
        '웹툰',
        '网漫',
        '網路漫畫',
        'Tira llarga',
        'Long Strip',
        'Long strip',
        'רצועה ארוכה',
        '롱스트립',
        '条漫',
        'ويب تون (Webtoon)',
        'وبتون',
        'Webtoon‏',
        'ウェブトゥーン',
        'Веб-комикс',
        'வெப்டூன்-',
        'Вебтун',
        'Cuộn dọc',
        '條漫',
    ],
    [MangaType.MANHWA]: [
        'Manhwa',
        'מנהווה',
        '만화',
        '韩漫',
        'Tira llarga',
        'Long Strip',
        'Long strip',
        'רצועה ארוכה',
        '롱스트립',
        '条漫',
        'مانهوا',
        'מנגה קוראנית (Manhwa)',
        '韓国マンガ',
        'Манхва',
        'மன்அ்வா',
        'Truyện Hàn',
        '韓漫',
    ],
    [MangaType.MANHUA]: [
        'Manhua',
        'מנהואה',
        '만화',
        '国漫',
        'Tira llarga',
        'Long Strip',
        'Long strip',
        'רצועה ארוכה',
        '롱스트립',
        '条漫',
        'مانها',
        'מנגה סינית (Manhua)',
        '漫画',
        'Маньхуа',
        'மன்உவா',
        'Truyện Trung',
        '國漫',
    ],
};""",
        """export const MANGA_TAGS_BY_MANGA_TYPE: Record<MangaType, string[]> = {
    [MangaType.MANGA]: ['Manga', 'מנגה', '만화', '漫画', 'مانجا', 'مانگا', 'Mangá', 'Манга', 'மங்கா-', 'Truyện Nhật'],
    [MangaType.COMIC]: ['Còmic', 'Comic', 'BD', 'קומיקס', 'Komik', '코믹', 'Quadrinho', '美漫'],
    [MangaType.WEBTOON]: [
        'Webtoon',
        'webtoon',
        'Webtoons',
        'וובטון',
        '웹툰',
        'Tira llarga',
        'Long Strip',
        'Long strip',
        'long strip',
        'LongStrip',
        'רצועה ארוכה',
        '롱스트립',
        '条漫',
        '條漫',
        '长图',
        '長圖',
        '竖屏',
        '豎屏',
        '竖版',
        '豎版',
        'Vertical',
        'ويب تون (Webtoon)',
        'وبتون',
        'Webtoon‏',
        'ウェブトゥーン',
        'Веб-комикс',
        'வெப்டூன்-',
        'Вебтун',
        'Cuộn dọc',
    ],
    [MangaType.MANHWA]: [
        'Tira llarga',
        'Long Strip',
        'Long strip',
        'long strip',
        'רצועה ארוכה',
        '롱스트립',
        '条漫',
        '條漫',
    ],
    [MangaType.MANHUA]: [
        'Tira llarga',
        'Long Strip',
        'Long strip',
        'long strip',
        'רצועה ארוכה',
        '롱스트립',
        '条漫',
        '條漫',
    ],
};""",
    )

    # 8a0b. Respect readingDirection / readingDirectionLock and persisted auto-webtoon detection in isAutoWebtoonMode
    reader_settings_utils = root / "src" / "features" / "reader" / "settings" / "ReaderSettings.utils.tsx"
    replace_exact(
        reader_settings_utils,
        "import type { MangaGenreInfo, MangaSourceNameInfo } from '@/features/manga/Manga.types.ts';\nimport { Mangas } from '@/features/manga/services/Mangas.ts';",
        "import type { MangaGenreInfo, MangaIdInfo, MangaMetaInfo, MangaSourceNameInfo } from '@/features/manga/Manga.types.ts';\nimport { Mangas } from '@/features/manga/services/Mangas.ts';\nimport { KomgaHostBridge } from '@/host/KomgaHostBridge.ts';",
    )
    replace_exact(
        reader_settings_utils,
        """export const isAutoWebtoonMode = (
    manga: MangaGenreInfo & MangaSourceNameInfo,
    shouldUseAutoWebtoonMode: IReaderSettings['shouldUseAutoWebtoonMode'],
    readingMode: IReaderSettingsWithDefaultFlag['readingMode'],
): boolean => shouldUseAutoWebtoonMode && readingMode.isDefault && Mangas.isLongStripType(manga);""",
        """export const isAutoWebtoonMode = (
    manga: MangaIdInfo & MangaGenreInfo & MangaSourceNameInfo & MangaMetaInfo,
    shouldUseAutoWebtoonMode: IReaderSettings['shouldUseAutoWebtoonMode'],
    readingMode: IReaderSettingsWithDefaultFlag['readingMode'],
): boolean => {
    if (!readingMode.isDefault) {
        return false;
    }
    const policy = KomgaHostBridge.getSeriesAutoWebtoonPolicy(manga.id);
    if (policy.suppressAutoWebtoon) {
        return false;
    }
    if (policy.forceWebtoon) {
        return true;
    }
    if (!shouldUseAutoWebtoonMode) {
        return false;
    }
    const detectedByRatio =
        KomgaHostBridge.isAutoWebtoonDetected(manga.id) ||
        Boolean(manga.meta?.some((meta) => meta.key === 'fk115_autoWebtoonDetected' && meta.value === '1'));
    return detectedByRatio || Mangas.isLongStripType(manga);
};""",
    )

    # 8a0b2. Avoid crushing tall Webtoon strips into 100dvh SCREEN height on desktop when user hasn't overridden pageScaleMode
    set_settings_hook = root / "src" / "features" / "reader" / "hooks" / "useReaderSetSettingsState.ts"
    replace_exact(
        set_settings_hook,
        "import { ReadingMode } from '@/features/reader/Reader.types.ts';",
        "import { ReaderPageScaleMode, ReadingMode } from '@/features/reader/Reader.types.ts';\nimport { extractOriginalKey } from '@/features/metadata/Metadata.utils.ts';",
    )
    replace_exact(
        set_settings_hook,
        """        const defaultSettingsWithAutoReadingMode = {
            ...defaultSettings,
            readingMode: shouldUseWebtoonMode ? ReadingMode.WEBTOON : defaultSettings.readingMode,
        };

        const profile = shouldUseWebtoonMode
            ? ReadingMode.WEBTOON
            : settingsWithDefaultProfileFallback.readingMode.value;""",
        """        const profile = shouldUseWebtoonMode
            ? ReadingMode.WEBTOON
            : settingsWithDefaultProfileFallback.readingMode.value;
        const hasExplicitGlobalPageScaleMode = Boolean(
            defaultSettingsMetadata &&
                Object.keys(defaultSettingsMetadata).some((key) => extractOriginalKey(key) === 'pageScaleMode'),
        );
        const defaultSettingsWithAutoReadingMode = {
            ...defaultSettings,
            readingMode: shouldUseWebtoonMode ? ReadingMode.WEBTOON : defaultSettings.readingMode,
            ...(profile === ReadingMode.WEBTOON && !hasExplicitGlobalPageScaleMode
                ? { pageScaleMode: ReaderPageScaleMode.WIDTH }
                : {}),
        };""",
    )

    # 8a0c. Expose naturalWidth/naturalHeight callback on page image load for aspect-ratio webtoon detection
    reader_utils = root / "src" / "features" / "reader" / "Reader.utils.ts"
    replace_exact(
        reader_utils,
        """export const createUpdateReaderPageLoadState =
    (
        actualPages: ReaderStatePages['pages'],
        setPagesToSpreadState: React.Dispatch<React.SetStateAction<ReaderPageSpreadState[]>>,
        setPageLoadStates: ReaderStatePages['setPageLoadStates'],
        setPageBackgroundColors: ReaderStatePages['setPageBackgroundColor'],
        readingMode: ReadingMode,
    ) =>
    (pagesIndex: number, url: string, isPrimary: boolean = true) => {
        if (pagesIndex > actualPages.length - 1) {
            return;
        }

        const page = actualPages[pagesIndex];
        const { index } = isPrimary ? page.primary : page.secondary!;

        const img = new Image();
        img.onload = () => {""",
        """export const createUpdateReaderPageLoadState =
    (
        actualPages: ReaderStatePages['pages'],
        setPagesToSpreadState: React.Dispatch<React.SetStateAction<ReaderPageSpreadState[]>>,
        setPageLoadStates: ReaderStatePages['setPageLoadStates'],
        setPageBackgroundColors: ReaderStatePages['setPageBackgroundColor'],
        readingMode: ReadingMode,
        onPageImageLoad?: (pageIndex: number, naturalWidth: number, naturalHeight: number) => void,
    ) =>
    (pagesIndex: number, url: string, isPrimary: boolean = true) => {
        if (pagesIndex > actualPages.length - 1) {
            return;
        }

        const page = actualPages[pagesIndex];
        const { index } = isPrimary ? page.primary : page.secondary!;

        const img = new Image();
        img.onload = () => {
            onPageImageLoad?.(index, img.naturalWidth, img.naturalHeight);""",
    )

    # 8a. Fill missing Simplified Chinese translation for upstream auto-crop setting
    zh_po = root / "src" / "i18n" / "locales" / "zh-Hans.po"
    zh_text = zh_po.read_text(encoding="utf-8")
    if 'msgid "Crop borders"' not in zh_text:
        zh_po.write_text(
            zh_text.rstrip()
            + '\n\n#: src/features/reader/settings/layout/ReaderLayoutSettings.tsx\n'
            + 'msgid "Crop borders"\n'
            + 'msgstr "自动裁切白边"\n',
            encoding="utf-8",
        )

    # 8b. Never auto-pop tap-zone overlay or reading-mode toast on initial reader open
    preview_hook = root / "src" / "features" / "reader" / "hooks" / "useReaderShowSettingPreviewOnChange.ts"
    replace_exact(
        preview_hook,
        """        if (isLoading || error || !areSettingsSet) {
            return;
        }

        const didReadingModeChange = JSON.stringify(readingMode) !== JSON.stringify(previousReadingMode.current);""",
        """        if (isLoading || error || !areSettingsSet) {
            return;
        }

        if (isInitialPreview.current) {
            previousReadingMode.current = readingMode;
            previousTapZoneLayout.current = tapZoneLayout;
            previousTapZoneInvertMode.current = tapZoneInvertMode;
            isInitialPreview.current = false;
            return;
        }

        const didReadingModeChange = JSON.stringify(readingMode) !== JSON.stringify(previousReadingMode.current);""",
    )

    # 9. ReaderService.useExit: return cleanly to /series/{seriesId} on fake-komga-115 Bookshelf
    reader_svc = root / "src" / "features" / "reader" / "services" / "ReaderService.ts"
    replace_exact(
        reader_svc,
        "import { useNavigate } from 'react-router-dom';\n",
        "",
    )
    replace_exact(
        reader_svc,
        "import { ReaderExitMode, ReaderOverlayMode, ReadingDirection, ReadingMode } from '@/features/reader/Reader.types.ts';",
        "import { ReaderOverlayMode, ReadingDirection, ReadingMode } from '@/features/reader/Reader.types.ts';",
    )
    replace_exact(
        reader_svc,
        "import { READER_REGEX, useBackButton } from '@/base/hooks/useBackButton.ts';",
        "import { READER_REGEX } from '@/base/hooks/useBackButton.ts';",
    )
    replace_exact(
        reader_svc,
        "import { UrlUtil } from '@/lib/UrlUtil.ts';",
        "import { UrlUtil } from '@/lib/UrlUtil.ts';\nimport { KomgaHostBridge } from '@/host/KomgaHostBridge.ts';",
    )
    replace_exact(
        reader_svc,
        """    static useExit(): () => void {
        const exitMode = useReaderSettingsStore('exitMode');
        const handleBack = useBackButton();
        const navigate = useNavigate();

        const openMangaPage = useCallback(() => {
            const { manga } = getReaderStore();

            if (!manga) {
                return () => {};
            }

            return navigate(AppRoutes.manga.path(manga.id));
        }, []);

        switch (exitMode) {
            case ReaderExitMode.PREVIOUS:
                return handleBack;
            case ReaderExitMode.MANGA:
                return openMangaPage;
            default:
                throw new Error(`Unexpected "exitMode" (${exitMode})`);
        }
    }""",
        """    static useExit(): () => void {
        return useCallback(() => {
            KomgaHostBridge.flushAllProgress();
            const { manga } = getReaderStore();
            const seriesId = manga ? KomgaHostBridge.getSeriesIdForManga(manga.id) : undefined;
            window.location.assign(seriesId ? `/series/${encodeURIComponent(seriesId)}` : '/');
        }, []);
    }""",
    )

    # 10. Streaming loading progress indicators & auto-webtoon front-5-page aspect-ratio probe
    chapter_viewer = root / "src" / "features" / "reader" / "viewer" / "ReaderChapterViewer.tsx"
    replace_exact(
        chapter_viewer,
        "import { LoadingPlaceholder } from '@/base/components/feedback/LoadingPlaceholder.tsx';",
        "import { MANGA_META_FIELDS } from '@/lib/graphql/manga/MangaFragments.ts';\nimport { KomgaHostBridge, isAutoWebtoonRatioTriggered, shouldRecordAutoWebtoonSample } from '@/host/KomgaHostBridge.ts';\nimport { BookIndexProgress } from '@/host/StreamingLoadStatus.tsx';",
    )
    replace_exact(
        chapter_viewer,
        "import { getReaderChaptersStore, getReaderPagesStore } from '@/features/reader/stores/ReaderStore.ts';",
        "import { getReaderChaptersStore, getReaderPagesStore, getReaderSettingsStore, getReaderStore } from '@/features/reader/stores/ReaderStore.ts';",
    )
    replace_exact(
        chapter_viewer,
        """    const onLoad = useMemo(
        () =>
            createUpdateReaderPageLoadState(
                actualPages,
                (value) => {
                    if (isCurrentChapterRef.current) {
                        getReaderPagesStore().setPageSpreadStates(value);
                    }

                    setPagesToSpreadState(value);
                },
                (value) => {
                    if (isCurrentChapterRef.current) {
                        getReaderPagesStore().setPageLoadStates(value);
                    }

                    setPageLoadStates(value);
                },
                (value) => {
                    if (isCurrentChapterRef.current) {
                        getReaderPagesStore().setPageBackgroundColor(value);
                    }

                    setPageBackgroundColors(value);
                },
                readingMode,
            ),
        [actualPages, readingMode],
    );""",
        """    const sampledWebtoonRatiosRef = useRef<Map<number, number>>(new Map());
    const autoWebtoonAppliedRef = useRef<boolean>(false);

    useEffect(() => {
        sampledWebtoonRatiosRef.current.clear();
        autoWebtoonAppliedRef.current = false;
    }, [chapterId]);

    const handlePageImageLoad = useCallback(
        (pageIndex: number, naturalWidth: number, naturalHeight: number) => {
            if (!isCurrentChapterRef.current || autoWebtoonAppliedRef.current) {
                return;
            }
            const { manga } = getReaderStore();
            const settings = getReaderSettingsStore();
            if (!manga || !settings.shouldUseAutoWebtoonMode || !settings.readingMode.isDefault) {
                return;
            }
            if (settings.readingMode.value === ReadingMode.WEBTOON) {
                return;
            }
            if (KomgaHostBridge.getSeriesAutoWebtoonPolicy(manga.id).suppressAutoWebtoon) {
                return;
            }
            const ratio = shouldRecordAutoWebtoonSample({
                pageIndex,
                naturalWidth,
                naturalHeight,
                totalPages,
            });
            if (ratio === null) {
                return;
            }
            sampledWebtoonRatiosRef.current.set(pageIndex, ratio);
            if (!isAutoWebtoonRatioTriggered(sampledWebtoonRatiosRef.current)) {
                return;
            }
            autoWebtoonAppliedRef.current = true;
            KomgaHostBridge.markAutoWebtoonDetected(manga.id);
            const { cache } = requestManager.graphQLClient.client;
            const cacheId = cache.identify({ __typename: 'MangaType', id: manga.id });
            if (cacheId) {
                const existingMeta = manga.meta ?? [];
                const nextMeta = [
                    ...existingMeta.filter((meta) => meta.key !== 'fk115_autoWebtoonDetected'),
                    {
                        __typename: 'MangaMetaType' as const,
                        mangaId: manga.id,
                        key: 'fk115_autoWebtoonDetected',
                        value: '1',
                    },
                ];
                cache.modify({
                    id: cacheId,
                    fields: {
                        meta() {
                            return nextMeta.map((meta) =>
                                cache.writeFragment({
                                    fragment: MANGA_META_FIELDS,
                                    data: {
                                        __typename: 'MangaMetaType',
                                        mangaId: manga.id,
                                        key: meta.key,
                                        value: meta.value,
                                    },
                                }),
                            );
                        },
                    },
                });
            }
        },
        [totalPages],
    );

    const onLoad = useMemo(
        () =>
            createUpdateReaderPageLoadState(
                actualPages,
                (value) => {
                    if (isCurrentChapterRef.current) {
                        getReaderPagesStore().setPageSpreadStates(value);
                    }

                    setPagesToSpreadState(value);
                },
                (value) => {
                    if (isCurrentChapterRef.current) {
                        getReaderPagesStore().setPageLoadStates(value);
                    }

                    setPageLoadStates(value);
                },
                (value) => {
                    if (isCurrentChapterRef.current) {
                        getReaderPagesStore().setPageBackgroundColor(value);
                    }

                    setPageBackgroundColors(value);
                },
                readingMode,
                handlePageImageLoad,
            ),
        [actualPages, readingMode, handlePageImageLoad],
    );""",
    )
    replace_exact(
        chapter_viewer,
        "                <LoadingPlaceholder />",
        "                <BookIndexProgress bookId={KomgaHostBridge.getBookIdForChapter(chapterId)} />",
    )

    spinner_img = root / "src" / "base" / "components" / "SpinnerImage.tsx"
    replace_exact(
        spinner_img,
        "import CircularProgress from '@mui/material/CircularProgress';",
        "import { PageLoadProgress } from '@/host/StreamingLoadStatus.tsx';",
    )
    replace_exact(
        spinner_img,
        "{isVisible && !!isLoading && <CircularProgress thickness={5} />}",
        "{isVisible && !!isLoading && <PageLoadProgress src={src} small={small} />}",
    )

    # 11. Keep reader loading screens & page placeholders consistent with pure black canvas
    reader_screen = root / "src" / "features" / "reader" / "screens" / "Reader.tsx"
    replace_exact(
        reader_screen,
        """            <Box
                sx={{
                    height: '100dvh',
                    width: '100dvw',
                    display: 'grid',
                    placeItems: 'center',
                }}
            >
                <LoadingPlaceholder />
            </Box>""",
        """            <Box
                sx={{
                    height: '100dvh',
                    width: '100dvw',
                    display: 'grid',
                    placeItems: 'center',
                    bgcolor: '#000',
                }}
            >
                <LoadingPlaceholder />
            </Box>""",
    )

    reader_page = root / "src" / "features" / "reader" / "viewer" / "components" / "ReaderPage.tsx"
    replace_exact(
        reader_page,
        """            spinnerStyle={{
                backgroundColor: 'background.paper',""",
        """            spinnerStyle={{
                backgroundColor: 'transparent',""",
    )


if __name__ == "__main__":
    main()
