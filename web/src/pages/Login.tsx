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
      <form className="card login-card" onSubmit={submit}>
        <div className="login-logo">
          <img src="/logo.svg" alt="fobe" className="logo-img big" />
        </div>
        <h1>{t('login_title')}</h1>
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
