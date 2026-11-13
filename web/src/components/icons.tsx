// icons.tsx — small inline stroke icons (16px, 1.5px stroke) for the
// sidebar and the file tree. Self-drawn primitives, no icon dependency.
import type { ReactNode } from "react";

function Svg({ children }: { children: ReactNode }): ReactNode {
  return (
    <svg width="16" height="16" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      {children}
    </svg>
  );
}

export const RepositoriesIcon = (): ReactNode => (
  <Svg>
    <path d="M2 4.75A1.75 1.75 0 0 1 3.75 3h2.9l1.6 2h4A1.75 1.75 0 0 1 14 6.75v4.5A1.75 1.75 0 0 1 12.25 13h-8.5A1.75 1.75 0 0 1 2 11.25z" />
  </Svg>
);

export const TokensIcon = (): ReactNode => (
  <Svg>
    <circle cx="5.75" cy="10.25" r="3.25" />
    <path d="M8.2 7.8 13 3M11 5l1.75-1.75M12.5 6.5l1.75-1.75" />
  </Svg>
);

export const OverviewIcon = (): ReactNode => (
  <Svg>
    <rect x="2.5" y="2.5" width="4.5" height="4.5" rx="1" />
    <rect x="9" y="2.5" width="4.5" height="4.5" rx="1" />
    <rect x="2.5" y="9" width="4.5" height="4.5" rx="1" />
    <rect x="9" y="9" width="4.5" height="4.5" rx="1" />
  </Svg>
);

export const CodeIcon = (): ReactNode => (
  <Svg>
    <path d="m5.5 5-3.5 3 3.5 3M10.5 5l3.5 3-3.5 3" />
  </Svg>
);

export const HistoryIcon = (): ReactNode => (
  <Svg>
    <circle cx="8" cy="8" r="5.75" />
    <path d="M8 4.75V8l2.5 1.75" />
  </Svg>
);

export const BranchesIcon = (): ReactNode => (
  <Svg>
    <circle cx="4.5" cy="3.75" r="1.75" />
    <circle cx="4.5" cy="12.25" r="1.75" />
    <circle cx="11.5" cy="6" r="1.75" />
    <path d="M4.5 5.5v5M6.25 6h1.5A1.75 1.75 0 0 1 9.5 7.75V10" />
  </Svg>
);

export const SubtreesIcon = (): ReactNode => (
  <Svg>
    <rect x="5.75" y="2" width="4.5" height="3.5" rx="1" />
    <rect x="2" y="10.5" width="4" height="3.5" rx="1" />
    <rect x="10" y="10.5" width="4" height="3.5" rx="1" />
    <path d="M8 5.5v2M4 10.5v-3h8v3" />
  </Svg>
);

export const SettingsIcon = (): ReactNode => (
  <Svg>
    <path d="M2.5 4.5h11M2.5 8h11M2.5 11.5h11" />
    <circle cx="10.5" cy="4.5" r="1.4" fill="var(--bg)" />
    <circle cx="5.5" cy="8" r="1.4" fill="var(--bg)" />
    <circle cx="9" cy="11.5" r="1.4" fill="var(--bg)" />
  </Svg>
);

export const FolderIcon = (): ReactNode => (
  <Svg>
    <path d="M1.75 4.75A1.75 1.75 0 0 1 3.5 3h2.75l1.5 2h5A1.75 1.75 0 0 1 14.5 6.75v5.5A1.75 1.75 0 0 1 12.75 14h-9.2A1.8 1.8 0 0 1 1.75 12.2z" />
  </Svg>
);

export const FileIcon16 = (): ReactNode => (
  <Svg>
    <path d="M4 1.75h5L12.25 5v8.25a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V2.75a1 1 0 0 1 1-1z" />
    <path d="M9 1.75V5h3.25" />
  </Svg>
);

export const LinkFileIcon = (): ReactNode => (
  <Svg>
    <path d="M6.5 3.5H11a1.5 1.5 0 0 1 1.5 1.5v7a1.5 1.5 0 0 1-1.5 1.5H5A1.5 1.5 0 0 1 3.5 12V6.5" />
    <path d="M1.5 8.5 6.5 3.5M3.5 3.5h3v3" />
  </Svg>
);

export const SubmoduleIcon = (): ReactNode => (
  <Svg>
    <rect x="2" y="5" width="12" height="7" rx="1.25" />
    <path d="m5.5 8.5 5-2.5M5.5 8.5 10.5 6M5.5 8.5l5 2.5" />
  </Svg>
);

export const ImportIcon = (): ReactNode => (
  <Svg>
    <path d="M8 2.5v7M5.25 6.75 8 9.5l2.75-2.75" />
    <path d="M2.5 10.5v1.75A1.25 1.25 0 0 0 3.75 13.5h8.5a1.25 1.25 0 0 0 1.25-1.25V10.5" />
  </Svg>
);
