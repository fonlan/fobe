import { useState, type FormEvent } from 'react';
import { Navigate, useNavigate } from 'react-router-dom';
import * as api from '../api';
import { apiErrorMessage } from '../api';
import { useAuth } from '../auth';
import { useI18n } from '../i18n';

export default function Login() {
  const { t } = useI18n();
  const { me, ready, setMe } = useAuth();
  const navigate = useNavigate();
  const [password, setPassword] = useState('');
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  if (ready && me) {
    return <Navigate to={me.must_change_password ? '/change-password' : '/'} replace />;
  }

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!password || busy) return;
    setBusy(true);
    setErr(null);
    try {
      const m = await api.login(password);
      setMe(m);
      navigate(m.must_change_password ? '/change-password' : '/', { replace: true });
    } catch (ex) {
      setErr(apiErrorMessage(ex, t));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="login-wrap">
      {/* The brand's pulse line, enlarged and breathing behind the card
          (styles.css: .login-pulse-*). Decorative only. The path echoes
          logo.svg's heartbeat — flat, spike, settle, flat — stretched across
          the viewport; its length (~1600) must stay under the dash period
          (1800) in the CSS so exactly one bright segment sweeps at a time. */}
      <div className="login-pulse" aria-hidden="true">
        <svg viewBox="0 0 1200 160" preserveAspectRatio="none">
          <path
            className="login-pulse-base"
            d="M0 84 H300 l22 -30 26 52 22 -36 30 14 18 -46 24 76 20 -46 H640 l16 -22 20 40 16 -26 h60 l14 -30 18 58 16 -32 H1200"
          />
          <path
            className="login-pulse-sweep"
            d="M0 84 H300 l22 -30 26 52 22 -36 30 14 18 -46 24 76 20 -46 H640 l16 -22 20 40 16 -26 h60 l14 -30 18 58 16 -32 H1200"
          />
        </svg>
      </div>
      <form className="card login-card" onSubmit={submit}>
        <div className="login-logo">
          <img src="/logo.svg" alt="fobe" className="logo-img big" />
        </div>
        <h1>{t('login_title')}</h1>
        <p className="login-tagline">Server Pulse</p>
        <label className="field">
          <span>{t('login_password')}</span>
          <input
            type="password"
            value={password}
            autoFocus
            onChange={(e) => setPassword(e.target.value)}
            placeholder="••••••••"
          />
        </label>
        {err && <div className="form-error">{err}</div>}
        <button className="btn primary block" type="submit" disabled={busy || password.length === 0}>
          {busy ? t('loading') : t('login_submit')}
        </button>
      </form>
    </div>
  );
}
