import { Navigate, Outlet, Route, Routes, useLocation } from 'react-router-dom';
import { useAuth } from './auth';
import { useI18n } from './i18n';
import Layout from './components/Layout';
import Login from './pages/Login';
import ChangePassword from './pages/ChangePassword';
import Overview from './pages/Overview';
import NodeDetail from './pages/NodeDetail';
import Targets from './pages/Targets';
import Servers from './pages/Servers';
import EditServer from './pages/EditServer';
import Alerts from './pages/Alerts';
import Audit from './pages/Audit';
import Notifications from './pages/Notifications';
import Settings from './pages/Settings';
import SettingsAI from './pages/SettingsAI';
import Subscriptions from './pages/Subscriptions';
import TerminalPage from './pages/TerminalPage';

/** Session gate: bounce to /login when anonymous, force password change first. */
function RequireAuth() {
  const { me, ready } = useAuth();
  const { t } = useI18n();
  const location = useLocation();

  if (!ready) return <div className="loading-full">{t('loading')}</div>;
  if (!me) return <Navigate to="/login" replace />;
  if (me.must_change_password && location.pathname !== '/change-password') {
    return <Navigate to="/change-password" replace />;
  }
  return <Outlet />;
}

export default function App() {
  return (
    <Routes>
      <Route path="/login" element={<Login />} />
      <Route element={<RequireAuth />}>
        <Route path="/change-password" element={<ChangePassword />} />
        <Route element={<Layout />}>
          <Route path="/" element={<Overview />} />
          <Route path="/nodes/:id" element={<NodeDetail />} />
          <Route path="/nodes/:id/terminal" element={<TerminalPage />} />
          <Route path="/alerts" element={<Alerts />} />
          <Route path="/settings" element={<Settings />}>
            <Route path="targets" element={<Targets />} />
            <Route path="servers" element={<Servers />} />
            <Route path="servers/:id" element={<EditServer />} />
            <Route path="subscriptions" element={<Subscriptions />} />
            <Route path="audit" element={<Audit />} />
            <Route path="notifications" element={<Notifications />} />
            <Route path="ai" element={<SettingsAI />} />
          </Route>
          <Route path="/targets" element={<Navigate to="/settings/targets" replace />} />
          <Route path="/subs" element={<Navigate to="/settings/subscriptions" replace />} />
          <Route path="/ai" element={<Navigate to="/" replace />} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Route>
      </Route>
    </Routes>
  );
}
