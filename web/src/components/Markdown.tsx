// Markdown for one assistant bubble (design §12.6 UI).
//
// react-markdown parses to React elements and never injects raw HTML (no
// rehype-raw), so model output cannot become markup — the transcript renders the
// text the model wrote, not markup it wrote. GFM adds the tables and
// strikethrough an ops answer actually uses.
//
// Images are deliberately NOT loaded: a Markdown image would make the browser
// fetch a third-party URL from inside the panel — a tracking pixel the model
// chose, and a leak of the panel's address — so the tag renders as a labelled
// placeholder instead (the same call fog makes for its chat bubbles).
//
// Streaming needs no special case: this component is a pure function of the text
// it is handed, so a half-written code fence renders as an open code block and
// completes on the next delta.

import { memo, useMemo } from 'react';
import ReactMarkdown, { type Components } from 'react-markdown';
import remarkGfm from 'remark-gfm';
import { useI18n } from '../i18n';

// Module scope on purpose: react-markdown compares this array by identity when
// it decides whether to rebuild its unified processor, so an inline
// `[remarkGfm]` would throw away the parse pipeline on every render — including
// the ones a stream of deltas causes.
const REMARK_PLUGINS = [remarkGfm];

export const Markdown = memo(function Markdown({ text }: { text: string }) {
  const { t } = useI18n();

  // Keyed on `t` so the two custom renderers keep one identity across those
  // re-renders: a fresh component function per render would make React unmount
  // and remount every link and every image placeholder instead of updating it.
  const components = useMemo<Components>(
    () => ({
      // No `src` is read at all, so a Markdown image cannot even reach the
      // network: the placeholder is the whole rendering of an `img` node.
      img: ({ alt }) => (
        <span className="ai-md-image" role="img" aria-label={alt || t('unknown')}>
          {t('ai_image_blocked', { alt: alt || t('unknown') })}
        </span>
      ),
      // Model-supplied links open in a new tab and never hand the panel's origin
      // to the target page as a referrer. The URL itself is still filtered by
      // react-markdown's default urlTransform, so a `javascript:` href cannot get
      // through even though it arrives as plain text in the answer.
      a: ({ href, children }) => (
        <a href={href} target="_blank" rel="noopener noreferrer">
          {children}
        </a>
      ),
    }),
    [t],
  );

  return (
    <div className="ai-md">
      <ReactMarkdown remarkPlugins={REMARK_PLUGINS} components={components}>
        {text}
      </ReactMarkdown>
    </div>
  );
});
