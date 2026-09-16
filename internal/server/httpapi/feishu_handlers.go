// 飞书 notification surface (design §15, 2026-09-16 修订): the scan-to-add
// registration session (feishureg), the channel test button and unbinding.
// The registration state itself lives in the feishureg.Manager (memory-only);
// these handlers only adapt it to the panel API.
package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/fobe-panel/fobe/internal/server/notify"
)

// handleFeishuQRStart begins a fresh scan-to-add attempt. Superseding an
// active one is deliberate: the QR is a UI affordance, restarting it must not
// require waiting for the old code to expire.
func (s *Server) handleFeishuQRStart(w http.ResponseWriter, r *http.Request) {
	if s.FeishuReg == nil {
		writeErr(w, http.StatusNotImplemented, "feishu_registration_unwired")
		return
	}
	st := s.FeishuReg.Start(s.background())
	s.audit("feishu_qr_start", "", s.Trust.RealIP(r))
	if st.State == "error" {
		writeErr(w, http.StatusBadGateway, st.Error)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleFeishuQRStatus(w http.ResponseWriter, r *http.Request) {
	if s.FeishuReg == nil {
		writeErr(w, http.StatusNotImplemented, "feishu_registration_unwired")
		return
	}
	writeJSON(w, http.StatusOK, s.FeishuReg.Status())
}

func (s *Server) handleFeishuQRCancel(w http.ResponseWriter, r *http.Request) {
	if s.FeishuReg == nil {
		writeErr(w, http.StatusNotImplemented, "feishu_registration_unwired")
		return
	}
	writeJSON(w, http.StatusOK, s.FeishuReg.Cancel())
}

// handleFeishuTest fires one real message through the configured 飞书
// sub-channel. The verdict (including the upstream's own message) comes back
// as data so the operator sees *why* a send failed — a permissions problem on
// a freshly scanned app looks very different from a wrong webhook signature.
func (s *Server) handleFeishuTest(w http.ResponseWriter, r *http.Request) {
	if s.Feishu == nil {
		writeErr(w, http.StatusNotImplemented, "feishu_not_wired")
		return
	}
	ev := notify.Event{
		Kind:      "notification",
		Event:     notify.EventTest,
		CreatedAt: time.Now().Unix(),
	}
	if err := s.Feishu.Deliver(ev); err != nil {
		if errors.Is(err, notify.ErrNotConfigured) {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "code": "feishu_not_configured"})
			return
		}
		detail := err.Error()
		s.Log.Warn("feishu test send failed", "err", detail)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "code": "feishu_test_failed", "detail": detail})
		return
	}
	s.audit("feishu_test_sent", "", s.Trust.RealIP(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleFeishuClear unbinds one sub-channel: mode=app drops the QR/manual app
// credentials, mode=webhook drops the custom-bot webhook. Rows are deleted
// outright — an empty settings row would still read as "configured".
func (s *Server) handleFeishuClear(w http.ResponseWriter, r *http.Request) {
	mode := strings.TrimSpace(r.URL.Query().Get("mode"))
	var keys []string
	switch mode {
	case "app":
		keys = []string{"notify.feishu_app_id", "notify.feishu_app_secret", "notify.feishu_receive_id", "notify.feishu_bot_name"}
	case "webhook":
		keys = []string{"notify.feishu_webhook_url", "notify.feishu_webhook_secret"}
	default:
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	for _, k := range keys {
		if err := s.Store.DeleteSetting(k); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal")
			return
		}
	}
	s.audit("feishu_cleared", mode, s.Trust.RealIP(r))
	s.publishEvent("settings_updated", "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
