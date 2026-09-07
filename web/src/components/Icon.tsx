import type { EntryType } from "../api/types";

interface IconProps {
  name: string;
  className?: string;
}

const PATHS: Record<string, string> = {
  folder: "M1.5 3.5A1.5 1.5 0 0 1 3 2h3.2l1.2 1.5H13A1.5 1.5 0 0 1 14.5 5v6.5A1.5 1.5 0 0 1 13 13H3a1.5 1.5 0 0 1-1.5-1.5v-8Z",
  file: "M4 1.5h5L12.5 5v9A1.5 1.5 0 0 1 11 15.5H4A1.5 1.5 0 0 1 2.5 14V3A1.5 1.5 0 0 1 4 1.5Zm4.75 1v3h3",
  archive: "M3 2h10a1 1 0 0 1 1 1v2H2V3a1 1 0 0 1 1-1Zm-1 4h12v7a1 1 0 0 1-1 1H3a1 1 0 0 1-1-1V6Zm5.25 1.5h1.5v1.5h-1.5V7.5Zm0 2.5h1.5v1.5h-1.5V10Z",
  link: "M6.5 9.5 9.5 6.5M6 4.5 7.6 3a2.7 2.7 0 1 1 3.8 3.8L9.9 8.3M10 11.5 8.4 13a2.7 2.7 0 1 1-3.8-3.8L6.1 7.7",
  search: "M7 12a5 5 0 1 1 0-10 5 5 0 0 1 0 10Zm3.7-1.3L14.5 14.5",
  chevron: "m6 4 4 4-4 4",
  chevronDown: "m4 6 4 4 4-4",
  gear: "M8 10.2a2.2 2.2 0 1 0 0-4.4 2.2 2.2 0 0 0 0 4.4Z M8 1.5l.8 1.6 1.8-.3.5 1.7 1.6.8-.9 1.6.9 1.6-1.6.8-.5 1.7-1.8-.3L8 14.5l-.8-1.6-1.8.3-.5-1.7-1.6-.8.9-1.6-.9-1.6 1.6-.8.5-1.7 1.8.3L8 1.5Z",
  refresh: "M13.5 8a5.5 5.5 0 1 1-1.6-3.9M13.5 2v3h-3",
  up: "M8 13V3.5M4 7l4-4 4 4",
  close: "m4 4 8 8M12 4l-8 8",
  warn: "M8 2.5 15 14H1L8 2.5Z M8 6.5v3.5 M8 11.8v.7",
};

export function Icon({ name, className }: IconProps) {
  const d = PATHS[name] ?? PATHS.file;
  return (
    <svg
      className={`icon${className ? ` ${className}` : ""}`}
      viewBox="0 0 16 16"
      width="14"
      height="14"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.3"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d={d} />
    </svg>
  );
}

export function EntryIcon({ type }: { type: EntryType }) {
  const name = type === "dir" ? "folder" : type === "archive" ? "archive" : type === "symlink" ? "link" : "file";
  return <Icon name={name} className={`icon-${name}`} />;
}
