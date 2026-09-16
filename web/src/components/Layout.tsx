import { Link, NavLink, Outlet, useNavigate } from 'react-router-dom';
import { useI18n } from '../i18n';
import { useTheme, type ThemeMode } from '../theme';
import { useAuth } from '../auth';
import * as api from '../api';

function BellIcon() {
  return (
    <svg viewBox="0 0 24 24" className="btn-icon" aria-hidden="true">
      <path d="M12 3a6 6 0 0 0-6 6v4l-2 3h16l-2-3V9a6 6 0 0 0-6-6z M10 19a2 2 0 0 0 4 0" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

function SettingsIcon() {
  return (
    <svg viewBox="0 0 24 24" className="btn-icon" aria-hidden="true">
      {/* Feather "settings" gear (MIT), scaled 0.82 about center so its ~1..23
          footprint lands on the same 3..21 box as the sibling icons; stroke
          compensated back to an effective 1.6 so line weight matches too.
          The previous hand-rolled path was 18x14 and read as squashed. */}
      <g transform="translate(12 12) scale(0.82) translate(-12 -12)">
        <circle cx="12" cy="12" r="3" fill="none" stroke="currentColor" strokeWidth="1.95" />
        <path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1 0 2.83 2 2 0 0 1-2.83 0l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-2 2 2 2 0 0 1-2-2v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83 0 2 2 0 0 1 0-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1-2-2 2 2 0 0 1 2-2h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 0-2.83 2 2 0 0 1 2.83 0l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 2-2 2 2 0 0 1 2 2v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 0 2 2 0 0 1 0 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 2 2 2 2 0 0 1-2 2h-.09a1.65 1.65 0 0 0-1.51 1z" fill="none" stroke="currentColor" strokeWidth="1.95" strokeLinecap="round" strokeLinejoin="round" />
      </g>
    </svg>
  );
}

function GlobeIcon() {
  return (
    <svg viewBox="0 0 24 24" className="btn-icon" aria-hidden="true">
      <circle cx="12" cy="12" r="9" fill="none" stroke="currentColor" strokeWidth="1.6" />
      <path d="M3 12h18 M12 3c2.7 2.4 4.2 5.5 4.2 9S14.7 18.6 12 21c-2.7-2.4-4.2-5.5-4.2-9S9.3 5.4 12 3z" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

function LogoutIcon() {
  return (
    <svg viewBox="0 0 24 24" className="btn-icon" aria-hidden="true">
      <path d="M9 21H6a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h3 M16 17l5-5-5-5 M21 12H9" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

export default function Layout() {
  const { t, locale, setLocale } = useI18n();
  const { mode, setMode, resolved } = useTheme();
  const { setMe } = useAuth();
  const navigate = useNavigate();

  const toggleLang = () => setLocale(locale === 'zh-CN' ? 'en-US' : 'zh-CN');

  const toggleTheme = () => {
    const order: ThemeMode[] = ['light', 'dark', 'system'];
    const next = order[(order.indexOf(mode) + 1) % order.length];
    setMode(next);
  };

  const doLogout = async () => {
    try {
      await api.logout();
    } catch {
      // cookie may already be gone; treat as logged out regardless
    }
    setMe(null);
    navigate('/login');
  };

  const themeLabel = mode === 'system' ? t('theme_system') : mode === 'dark' ? t('theme_dark') : t('theme_light');

  return (
    <div className="shell">
      <div className="main">
        <header className="topbar">
          <Link to="/" className="topbar-brand" aria-label={t('app_name')}>
            <img src="/logo.svg" alt="" className="logo-img" />
            <span>Fobe</span>
          </Link>
          <div className="topbar-actions">
            <NavLink to="/alerts" className="btn ghost small icon-link" title={t('nav_alerts')} aria-label={t('nav_alerts')}>
              <BellIcon />
              <span className="btn-text">{t('nav_alerts')}</span>
            </NavLink>
            <NavLink to="/settings" className="btn ghost small icon-link" title={t('nav_settings')} aria-label={t('nav_settings')}>
              <SettingsIcon />
              <span className="btn-text">{t('nav_settings')}</span>
            </NavLink>
            <button type="button" className="btn ghost small" onClick={toggleLang} title={t('language')} aria-label={t('language')}>
              <GlobeIcon />
              <span className="btn-text">{locale === 'zh-CN' ? 'EN' : '中文'}</span>
            </button>
            <button type="button" className="btn ghost small theme-control" onClick={toggleTheme} title={t('theme')} aria-label={t('theme')}>
              {resolved === 'dark' ? (
                <svg viewBox="0 0 24 24" className="btn-icon" aria-hidden="true">
                  <path d="M21 12.8A8.5 8.5 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" />
                </svg>
              ) : (
                <svg viewBox="0 0 24 24" className="btn-icon" aria-hidden="true">
                  <circle cx="12" cy="12" r="4" fill="none" stroke="currentColor" strokeWidth="1.6" />
                  <path d="M12 2v2 M12 20v2 M2 12h2 M20 12h2 M4.9 4.9l1.4 1.4 M17.7 17.7l1.4 1.4 M4.9 19.1l1.4-1.4 M17.7 6.3l1.4-1.4" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
                </svg>
              )}
              <span className="btn-text">{themeLabel}</span>
            </button>
            <button type="button" className="btn ghost small" onClick={() => void doLogout()} title={t('logout')} aria-label={t('logout')}>
              <LogoutIcon />
              <span className="btn-text">{t('logout')}</span>
            </button>
          </div>
        </header>
        <main className="content">
          <Outlet />
        </main>
      </div>
    </div>
  );
}
