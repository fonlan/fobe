// Shared load-failure card: the error text plus a retry button wired to the
// page's own reload thunk.
import { useI18n } from '../i18n';

export function ErrorState({ message, onRetry }: { message: string; onRetry: () => void }) {
  const { t } = useI18n();
  return (
    <div className="card error-card">
      <p>{message}</p>
      <button type="button" className="btn" onClick={onRetry}>
        {t('retry')}
      </button>
    </div>
  );
}
