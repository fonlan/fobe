import { useI18n } from '../i18n';

export default function ComingSoon({ titleKey }: { titleKey: string }) {
  const { t } = useI18n();
  return (
    <div className="coming-soon">
      <h2>{t(titleKey)}</h2>
      <p className="hint">{t('coming_soon_desc')}</p>
      <span className="chip">{t('coming_soon')}</span>
    </div>
  );
}
