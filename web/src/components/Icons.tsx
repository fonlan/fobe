import type { ReactNode, SVGProps } from 'react';

/**
 * Inline SVG icons for row actions (no icon library and no CDN: §11 keeps every
 * asset self-hosted, and a handful of 24×24 stroke paths is cheaper than a
 * dependency).
 *
 * Every icon is decorative: the button around it carries the accessible name
 * (aria-label / title), because two icon-only buttons in the same row are
 * otherwise indistinguishable to a screen reader.
 */
type IconProps = SVGProps<SVGSVGElement>;

function Base({ children, className, ...rest }: IconProps & { children: ReactNode }) {
  return (
    <svg
      viewBox="0 0 24 24"
      className={'btn-icon' + (className ? ' ' + className : '')}
      fill="none"
      stroke="currentColor"
      strokeWidth="1.6"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      focusable="false"
      {...rest}
    >
      {children}
    </svg>
  );
}

/** Delete a cached release from the server's disk. */
export function TrashIcon(props: IconProps) {
  return (
    <Base {...props}>
      <path d="M4 7h16M10 7V5h4v2M6 7l1 13h10l1-13M10 11v6M14 11v6" />
    </Base>
  );
}

/** Publish a cached release to every sing-box node. */
export function PublishIcon(props: IconProps) {
  return (
    <Base {...props}>
      <path d="M12 3c3.5 1.8 5.5 5 5.5 9L12 15l-5.5-3C6.5 8 8.5 4.8 12 3zM12 15v6M9.5 21h5M7 13.5 5 16l2.5.5M17 13.5l2 2.5-2.5.5" />
    </Base>
  );
}

/** Fetch a release into the server's cache. */
export function DownloadIcon(props: IconProps) {
  return (
    <Base {...props}>
      <path d="M12 4v10m0 0-4-4m4 4 4-4M5 19h14" />
    </Base>
  );
}

/** Reload the upstream release listing after a failed refresh. */
export function RefreshIcon(props: IconProps) {
  return (
    <Base {...props}>
      <path d="M20 12a8 8 0 1 1-2.3-5.7M20 4v5h-5" />
    </Base>
  );
}

/** Retry a download that failed before publishing. */
export function RetryIcon(props: IconProps) {
  return (
    <Base {...props}>
      <path d="M4 12a8 8 0 1 0 2.3-5.7M4 4v5h5" />
    </Base>
  );
}

/** Copy a server's primary IP (settings → servers table). */
export function CopyIcon(props: IconProps) {
  return (
    <Base {...props}>
      <path d="M9 9h10v10H9zM15 9V5H5v10h4" />
    </Base>
  );
}

/** Edit a server's settings. */
export function PencilIcon(props: IconProps) {
  return (
    <Base {...props}>
      <path d="M4 20h4L19.5 8.5a2.1 2.1 0 0 0-3-3L5 17v3zM14 7l3 3" />
    </Base>
  );
}
