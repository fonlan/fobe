import { flagEmoji } from '../format';

/** Country flag emoji from ISO code; gray dot when unknown/empty. */
export default function Flag({ cc }: { cc: string }) {
  const emoji = flagEmoji(cc);
  if (!emoji) return <span className="flag flag-none" aria-label="unknown country" />;
  return <span className="flag" role="img">{emoji}</span>;
}
