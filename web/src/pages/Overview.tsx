// Overview: node card wall with live refresh via /ws/events (10s polling
// fallback), plus the "add node" dialog (one-time reg token + install command).

import { useCallback, useEffect, useState, type FormEvent } from 'react';
import * as api from '../api';
import { apiErrorMessage } from '../api';
import { useI18n } from '../i18n';
import type { NodeView, RegTokenInfo } from '../types';
import NodeCard from '../components/NodeCard';
import Modal from '../components/Modal';
import { copyText } from '../format';

type AddStep = 'closed' | 'form' | 'done';

export default function Overview() {
  const { t } = useI18n();
  const [nodes, setNodes] = useState<NodeView[] | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [addStep, setAddStep] = useState<AddStep>('closed');

  const load = useCallback(async () => {
    try {
      const r = await api.listNodes();
      setNodes(r.nodes ?? []);
      setErr(null);
    } catch (e) {
      setErr(apiErrorMessage(e, t));
    }
  }, [t]);

  useEffect(() => {
    void load();
  }, [load]);

  // live updates: push events when available, poll every 10s regardless
  useEffect(() => {
    const close = api.openEvents(() => void load());
    const timer = window.setInterval(() => void load(), 10000);
    return () => {
      close();
      window.clearInterval(timer);
    };
  }, [load]);

  return (
    <div>
      <div className="page-head">
        <h2>
          {t('nav_overview')}
          {nodes && <span className="count-chip">{t('nodes_count', { n: nodes.length })}</span>}
        </h2>
        <button type="button" className="btn primary" onClick={() => setAddStep('form')}>
          + {t('add_node')}
        </button>
      </div>

      {err && (
        <div className="card error-card">
          <p>{err}</p>
          <button type="button" className="btn" onClick={() => void load()}>
            {t('retry')}
          </button>
        </div>
      )}

      {!err && nodes === null && <div className="loading">{t('loading')}</div>}

      {nodes !== null && nodes.length === 0 && <div className="empty-hint">{t('no_nodes')}</div>}

      {nodes !== null && nodes.length > 0 && (
        <div className="grid">
          {nodes.map((n) => (
            <NodeCard key={n.id} node={n} />
          ))}
        </div>
      )}

      {addStep !== 'closed' && <AddNodeModal step={addStep} setStep={setAddStep} onCreated={() => void load()} />}
    </div>
  );
}

function AddNodeModal({
  step,
  setStep,
  onCreated,
}: {
  step: AddStep;
  setStep: (s: AddStep) => void;
  onCreated: () => void;
}) {
  const { t } = useI18n();
  const [name, setName] = useState('');
  const [note, setNote] = useState('');
  const [info, setInfo] = useState<RegTokenInfo | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);

  const create = async (e: FormEvent) => {
    e.preventDefault();
    if (busy) return;
    setBusy(true);
    setErr(null);
    const trimmedName = name.trim();
    if (!trimmedName) {
      setErr(t('err_name_required'));
      setBusy(false);
      return;
    }
    try {
      const r = await api.createRegToken(trimmedName, note.trim());
      setInfo(r);
      setStep('done');
      onCreated();
    } catch (ex) {
      setErr(apiErrorMessage(ex, t));
    } finally {
      setBusy(false);
    }
  };

  const doCopy = async () => {
    if (!info) return;
    const ok = await copyText(info.install_command);
    setCopied(ok);
    if (ok) window.setTimeout(() => setCopied(false), 2000);
  };

  return (
    <Modal title={t('add_node_title')} onClose={() => setStep('closed')}>
      {step === 'form' && (
        <form onSubmit={create} className="stack">
          <label className="field">
            <span>{t('add_node_name_label')}</span>
            <input
              value={name}
              autoFocus
              required
              maxLength={120}
              onChange={(e) => setName(e.target.value)}
            />
          </label>
          <label className="field">
            <span>{t('add_node_note_label')}</span>
            <input value={note} onChange={(e) => setNote(e.target.value)} />
          </label>
          {err && <div className="form-error">{err}</div>}
          <div className="row-end">
            <button type="button" className="btn" onClick={() => setStep('closed')}>
              {t('cancel')}
            </button>
            <button type="submit" className="btn primary" disabled={busy}>
              {busy ? t('loading') : t('add_node_create')}
            </button>
          </div>
        </form>
      )}
      {step === 'done' && info && (
        <div className="stack">
          <p className="hint">{t('install_cmd_warn', { min: Math.round(info.ttl / 60) })}</p>
          <pre className="code-block">{info.install_command}</pre>
          <div className="row-end">
            <button type="button" className="btn" onClick={() => void doCopy()}>
              {copied ? t('copied') : t('copy')}
            </button>
            <button
              type="button"
              className="btn"
              onClick={() => {
                setInfo(null);
                setName('');
                setNote('');
                setStep('form');
              }}
            >
              {t('add_another')}
            </button>
            <button type="button" className="btn primary" onClick={() => setStep('closed')}>
              {t('close')}
            </button>
          </div>
        </div>
      )}
    </Modal>
  );
}
