import { createRoot } from 'react-dom/client';
import { BrowserRouter } from 'react-router-dom';
import App from './App';
import { AuthProvider } from './auth';
import { I18nProvider } from './i18n';
import { ThemeProvider } from './theme';
import './styles.css';

const rootEl = document.getElementById('root');
if (!rootEl) throw new Error('#root not found');

// No StrictMode: its dev-only double mount opens/closes the terminal
// WebSocket twice, and the interleaved open(A)/close(A)/open(B) frames can
// tear down a live PTY session on the agent.
createRoot(rootEl).render(
  <ThemeProvider>
    <I18nProvider>
      <AuthProvider>
        <BrowserRouter>
          <App />
        </BrowserRouter>
      </AuthProvider>
    </I18nProvider>
  </ThemeProvider>,
);
