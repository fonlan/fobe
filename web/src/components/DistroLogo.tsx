/**
 * Self-hosted distro badges (invariant #11: no CDN). Each mark is a
 * deliberately simplified, hand-drawn SVG in the distro's brand colour —
 * recognizable at tile size without shipping anyone's trademark artwork.
 * Unknown IDs fall back to a neutral badge with the ID's first letter.
 */

// Aliases collapse the ids distributions actually ship in /etc/os-release.
const ALIASES: Record<string, string> = {
  'opensuse-leap': 'opensuse',
  'opensuse-tumbleweed': 'opensuse',
  suse: 'opensuse',
  alma: 'almalinux',
  ol: 'oracle',
  oracle: 'oracle',
  amzn: 'amazon',
  'amazon-linux': 'amazon',
  rhel: 'rhel',
  redhat: 'rhel',
  'red-hat': 'rhel',
  'raspberry-pi-os': 'raspbian',
};

export const KNOWN_DISTROS: Record<string, string> = {
  debian: 'Debian',
  ubuntu: 'Ubuntu',
  alpine: 'Alpine',
  arch: 'Arch Linux',
  centos: 'CentOS',
  rocky: 'Rocky Linux',
  almalinux: 'AlmaLinux',
  fedora: 'Fedora',
  opensuse: 'openSUSE',
  rhel: 'RHEL',
  oracle: 'Oracle Linux',
  amazon: 'Amazon Linux',
  raspbian: 'Raspberry Pi OS',
  openwrt: 'OpenWrt',
  kali: 'Kali Linux',
  nixos: 'NixOS',
};

/** Display name for an os-release ID; unknown ids get capitalized. */
export function distroName(id: string): string {
  const key = ALIASES[id.toLowerCase()] ?? id.toLowerCase();
  return KNOWN_DISTROS[key] ?? (id ? id.charAt(0).toUpperCase() + id.slice(1) : '-');
}

export default function DistroLogo({ id, size = 18 }: { id: string; size?: number }) {
  const key = ALIASES[id.toLowerCase()] ?? id.toLowerCase();
  const mark = MARKS[key];
  const style = { width: size, height: size, flex: 'none' } as const;
  if (!mark) {
    const letter = (id || '?').charAt(0).toUpperCase();
    return (
      <svg viewBox="0 0 24 24" style={style} role="img" aria-label={id}>
        <rect x="1" y="1" width="22" height="22" rx="6" fill="#546E7A" />
        <text x="12" y="16.5" textAnchor="middle" fontSize="12" fontWeight="700" fill="#fff">
          {letter}
        </text>
      </svg>
    );
  }
  return (
    <svg viewBox="0 0 24 24" style={style} role="img" aria-label={distroName(id)}>
      <rect x="1" y="1" width="22" height="22" rx="6" fill={mark.bg} />
      {mark.paths}
    </svg>
  );
}

interface Mark {
  bg: string;
  paths: JSX.Element;
}

const S = { fill: 'none', stroke: '#fff', strokeWidth: 1.9, strokeLinecap: 'round' as const, strokeLinejoin: 'round' as const };

const MARKS: Record<string, Mark> = {
  debian: {
    bg: '#D70A53',
    paths: (
      <path
        {...S}
        d="M12 6.2c3.7 0 5.8 2.4 5.8 5.2s-2.1 5.2-5 5.2-5-2-5-4.6 1.9-4.3 4.2-4.3 3.8 1.6 3.8 3.4-1.3 3-3 3"
      />
    ),
  },
  ubuntu: {
    bg: '#E95420',
    paths: (
      <>
        <circle cx="12" cy="12" r="2.4" fill="none" stroke="#fff" strokeWidth="1.7" />
        {[[12, 4.8], [5.9, 15.6], [18.1, 15.6]].map(([x, y]) => (
          <g key={`${x}-${y}`}>
            <line x1="12" y1="12" x2={x} y2={y} stroke="#fff" strokeWidth="1.7" />
            <circle cx={x} cy={y} r="1.9" fill="#fff" />
          </g>
        ))}
      </>
    ),
  },
  alpine: {
    bg: '#0D597F',
    paths: <path d="M4 17.5 L10.2 7.5 L13.2 12.3 L15.2 9.2 L20 17.5 Z" fill="#fff" />,
  },
  arch: {
    bg: '#1793D1',
    paths: <path d="M12 4.2 L19.2 19.8 H14.6 L12 13.9 L9.4 19.8 H4.8 Z" fill="#fff" />,
  },
  centos: {
    bg: '#262577',
    paths: (
      <>
        <rect x="5" y="5" width="6.2" height="6.2" rx="1" fill="#fff" />
        <rect x="12.8" y="5" width="6.2" height="6.2" rx="1" fill="#9CCD2A" />
        <rect x="5" y="12.8" width="6.2" height="6.2" rx="1" fill="#9CCD2A" />
        <rect x="12.8" y="12.8" width="6.2" height="6.2" rx="1" fill="#fff" />
      </>
    ),
  },
  rocky: {
    bg: '#10B981',
    paths: <path d="M3.5 17.5 L9.3 8.2 L12.4 12.8 L14.9 9.2 L20.5 17.5 Z" fill="#fff" />,
  },
  almalinux: {
    bg: '#0E4DC6',
    paths: <path d="M12 5 L19 19 H5 Z" {...S} />,
  },
  fedora: {
    bg: '#51A2DA',
    paths: (
      <>
        <path {...S} strokeWidth="2.3" d="M15 5.8c-2.9 0-4.2 1.9-4.2 4.2v8.4" />
        <path {...S} strokeWidth="2.3" d="M7.8 11.6h5.4" />
      </>
    ),
  },
  opensuse: {
    bg: '#73BA25',
    paths: (
      <>
        <path {...S} d="M4.5 14.8c3 2.6 8.2 3.1 12.2 1s4.6-5.4 3.4-8" />
        <circle cx="9" cy="9.2" r="2.2" fill="none" stroke="#fff" strokeWidth="1.5" />
        <circle cx="9" cy="9.2" r="0.9" fill="#fff" />
      </>
    ),
  },
  rhel: {
    bg: '#EE0000',
    paths: (
      <>
        <path d="M8 14.5v-2.8c0-2.6 1.8-4.2 4-4.2s4 1.6 4 4.2v2.8Z" fill="#fff" />
        <ellipse cx="12" cy="15.4" rx="7.2" ry="1.9" fill="#fff" />
      </>
    ),
  },
  oracle: {
    bg: '#C74634',
    paths: <circle cx="12" cy="12" r="5.4" fill="none" stroke="#fff" strokeWidth="2.4" />,
  },
  amazon: {
    bg: '#232F3E',
    paths: (
      <>
        <path d="M5.5 11.2c1.8 2.5 4 3.7 6.5 3.7s4.7-1.2 6.5-3.7" fill="none" stroke="#FF9900" strokeWidth="1.9" strokeLinecap="round" />
        <path d="M17 9.7l2 1.6-2.5.8" fill="none" stroke="#FF9900" strokeWidth="1.9" strokeLinecap="round" strokeLinejoin="round" />
      </>
    ),
  },
  raspbian: {
    bg: '#C51A4A',
    paths: (
      <>
        <path d="M12 6.4c-1.3-1.9-3.7-2-4.9-1 1.3 1.7 3.4 2 4.9 1Zm0 0c1.3-1.9 3.7-2 4.9-1-1.3 1.7-3.4 2-4.9 1Z" fill="#75A928" />
        <circle cx="9" cy="12.4" r="2.1" fill="#fff" />
        <circle cx="15" cy="12.4" r="2.1" fill="#fff" />
        <circle cx="12" cy="16.4" r="2.1" fill="#fff" />
      </>
    ),
  },
  openwrt: {
    bg: '#00B5E2',
    paths: (
      <>
        <circle cx="12" cy="16.8" r="1.7" fill="#fff" />
        <path {...S} d="M7.6 12.7a6.2 6.2 0 0 1 8.8 0" />
        <path {...S} d="M4.9 9.8a10 10 0 0 1 14.2 0" />
      </>
    ),
  },
  kali: {
    bg: '#2A6FEA',
    paths: (
      <>
        <path {...S} strokeWidth="2.3" d="M8.8 5v14" />
        <path {...S} strokeWidth="2.3" d="M15.6 5.2 L10.6 12 l5 6.8" />
      </>
    ),
  },
  nixos: {
    bg: '#5277C3',
    paths: <path {...S} strokeWidth="1.7" d="M12 5v14 M6.1 8.5l11.8 7 M17.9 8.5l-11.8 7" />,
  },
};
