import { useState, type FormEvent } from 'react';
import { useNavigate } from 'react-router-dom';
import * as api from '../api';
import { apiErrorMessage } from '../api';
import { useAuth } from '../auth';
import { useI18n } from '../i18n';

export default function ChangePassword() {
  const { t } = useI18n();
  const { me, setMe, refresh } = useAuth();
  const navigate = useNavigate();
  const [oldPwd, setOldPwd] = useState('');
  const [newPwd, setNewPwd] = useState('');
  const [confirm, setConfirm] = useState('');
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (busy) return;
    if (newPwd.length < 8) {
      setErr(t('err_password_too_short'));
      return;
    }
    if (newPwd !== confirm) {
      setErr(t('cp_mismatch'));
      return;
    }
    setBusy(true);
    setErr(null);
    try {
      await api.changePassword(oldPwd, newPwd);
      setMe(me ? { ...me, must_change_password: false } : null);
      await refresh();
      navigate('/', { replace: true });
    } catch (ex) {
      setErr(apiErrorMessage(ex, t));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="login-wrap">
      <form className="card login-card" onSubmit={submit}>
        <h1>{t('cp_title')}</h1>
        {me?.must_change_password && <p className="hint">{t('cp_forced')}</p>}
        <label className="field">
          <span>{t('cp_old')}</span>
          <input type="password" value={oldPwd} autoFocus onChange={(e) => setOldPwd(e.target.value)} />
        </label>
        <label className="field">
          <span>{t('cp_new')}</span>
          <input type="password" value={newPwd} onChange={(e) => setNewPwd(e.target.value)} />
        </label>
        <label className="field">
          <span>{t('cp_confirm')}</span>
          <input type="password" value={confirm} onChange={(e) => setConfirm(e.target.value)} />
        </label>
        {err && <div className="form-error">{err}</div>}
        <button className="btn primary block" type="submit" disabled={busy}>
          {busy ? t('loading') : t('cp_submit')}
        </button>
      </form>
    </div>
  );
}
