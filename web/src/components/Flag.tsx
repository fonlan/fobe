import { useState } from 'react';

/**
 * Country flag from a self-hosted SVG (§14), never an emoji glyph: emoji flags
 * are a platform font decision, and those disagree — Windows has no flag
 * glyphs at all (every country prints as its two letters), and Apple's zh-CN
 * font draws the TW sequence as a crossed box. An asset is the only way the
 * same panel shows the same flag everywhere.
 *
 * The artwork is the 4:3 set vendored from flag-icons (see flags/LICENSE.txt).
 */
export default function Flag({ cc }: { cc: string }) {
  const up = cc.trim().toUpperCase();
  // Remember only the code that failed to load: typing through a bogus
  // intermediate value ("QQ") must not leave a gray swatch behind after the
  // operator fixes it, and `cc` changes on every keystroke of the edit form.
  const [failed, setFailed] = useState('');
  // "XX" is the panel's own unknown-country code (§14 / subscription names),
  // so it gets the same gray swatch instead of fetching the vendor's crossed
  // placeholder flag — one "no country" look for every unresolvable value.
  if (!/^[A-Z]{2}$/.test(up) || up === 'XX' || failed === up) {
    return <span className="flag flag-none" aria-label="unknown country" />;
  }
  return (
    <img
      className="flag"
      src={`/flags/${up.toLowerCase()}.svg`}
      alt={up}
      onError={() => setFailed(up)}
    />
  );
}
