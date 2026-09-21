package httpapi

// nftables port forwarding (design.md §21).
//
// The panel edits the probe's live ruleset through one-shot commands, not
// through desired state: the ruleset is shared with other tools (nfpf.sh,
// hand-written nft), so the panel must never declare "the complete set" — it
// would delete whatever it did not know about. Each mutation is one targeted
// nft transaction on the agent, and every read comes back either from the last
// state frame or from an explicit `list` round trip.

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fonlan/fobe/internal/protocol"
	"github.com/fonlan/fobe/internal/server/store"
)

const (
	// forwardsWait bounds how long a panel action blocks on the probe. An nft
	// transaction is fast; this budget is for a slow link, and running out of it
	// is not a failure — the queued command keeps its 10-minute TTL.
	forwardsWait = 8 * time.Second
	// forwardsMaxRule caps the stored/reported rule list so a pathological
	// ruleset cannot turn a state frame into an unbounded write.
	forwardsMaxRule = 512
)

// forwardView is one row of the panel's port-forward table. handle is passed
// back on edit/delete so the agent can act on that exact rule even when two
// rules share a tuple; extra_match means the rule carries match terms the panel
// does not model (source address, counters, …) and must not be rewritten.
type forwardView struct {
	Proto      string `json:"proto"`
	SrcPort    int    `json:"src_port"`
	Iface      string `json:"iface"`
	DstIP      string `json:"dst_ip"`
	DstPort    int    `json:"dst_port"`
	Comment    string `json:"comment"`
	Handle     int    `json:"handle"`
	ExtraMatch bool   `json:"extra_match"`
}

// forwardsReply is the whole panel-facing state: the capability report, the
// rules, and how fresh they are.
type forwardsReply struct {
	Supported   bool   `json:"supported"`
	Initialized bool   `json:"initialized"`
	Code        string `json:"code"`
	Message     string `json:"message"`
	ReportedAt  int64  `json:"reported_at"`
	// AgentSupported is false until a probe has ever reported §21 state, i.e.
	// until its agent is new enough to understand the command kind.
	AgentSupported bool          `json:"agent_supported"`
	Forwards       []forwardView `json:"forwards"`
	// Live: this payload came from a round trip, not from the stored snapshot.
	Live bool `json:"live"`
	// Queued: the command is still in flight (offline probe, or slower than
	// forwardsWait). TTL 10 minutes; the list self-heals via state frames.
	Queued   bool     `json:"queued"`
	Warnings []string `json:"warnings,omitempty"`
}

// forwardRuleReq is the wire shape the panel sends for add/update/delete.
type forwardRuleReq struct {
	Proto   string `json:"proto"`
	SrcPort int    `json:"src_port"`
	Iface   string `json:"iface"`
	DstIP   string `json:"dst_ip"`
	DstPort int    `json:"dst_port"`
	// Comment is written onto the DNAT rule the way nfpf.sh does it (after
	// `dnat to`, which nft accepts when the rule arrives through `nft -f -`).
	Comment string `json:"comment"`
	Handle  int    `json:"handle"`
}

type forwardAddReq struct {
	Rule forwardRuleReq `json:"rule"`
}

type forwardUpdateReq struct {
	Old  forwardRuleReq `json:"old"`
	Rule forwardRuleReq `json:"rule"`
}

type forwardDeleteReq struct {
	Rule forwardRuleReq `json:"rule"`
}

func (r forwardRuleReq) toProtocol() protocol.ForwardRule {
	return protocol.ForwardRule{
		Proto: strings.ToLower(strings.TrimSpace(r.Proto)), SrcPort: r.SrcPort,
		Iface: strings.TrimSpace(r.Iface), DstIP: strings.TrimSpace(r.DstIP),
		DstPort: r.DstPort, Handle: r.Handle,
		Comment: strings.TrimSpace(r.Comment),
	}
}

// validateForwardReq is the cheap server-side gate: it only rejects shapes that
// are certainly wrong, because the agent owns the authoritative validation
// (interface existence, port ranges, conflicts with rules the panel cannot
// see). Rejecting here keeps garbage out of the audit trail and the queue.
func validateForwardReq(r forwardRuleReq) string {
	p := r.toProtocol()
	if p.Proto != "tcp" && p.Proto != "udp" {
		return "bad_forward"
	}
	if p.SrcPort < 1 || p.SrcPort > 65535 || p.DstPort < 1 || p.DstPort > 65535 {
		return "bad_forward"
	}
	if ip := net.ParseIP(p.DstIP); ip == nil || ip.To4() == nil {
		return "bad_forward"
	}
	if len(p.Iface) > 32 {
		return "bad_forward"
	}
	// nft string literals have no escapes, so a quote in a comment cannot be
	// written at all; the agent refuses it too — catch it before the queue.
	if len([]rune(p.Comment)) > 128 || strings.ContainsAny(p.Comment, "\"\n\r\t") {
		return "bad_comment"
	}
	return ""
}

// handleListNodeForwards returns the stored snapshot, or a fresh one with
// ?live=1. The stored snapshot is the default because a page load must not
// block on a probe (offline probes answer instantly from here).
func (s *Server) handleListNodeForwards(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.GetNode(id); !writeStoreErr(w, err) {
		return
	}
	reply, err := s.forwardsReply(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	if r.URL.Query().Get("live") == "1" {
		res, queued, ferr := s.forwardsRoundTrip(r, id, protocol.ForwardsRequest{Action: protocol.ForwardsList},
			"refresh nftables port forwards")
		reply.Queued = queued
		if ferr == nil && res != nil {
			s.Hub.RecordForwardState(id, &res.State)
			reply, err = s.forwardsReply(id)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "internal")
				return
			}
			reply.Live = true
		}
	}
	writeJSON(w, http.StatusOK, reply)
}

// handleAddNodeForward adds one forward to the probe (§21).
func (s *Server) handleAddNodeForward(w http.ResponseWriter, r *http.Request) {
	id, ok := s.forwardsNode(w, r)
	if !ok {
		return
	}
	var req forwardAddReq
	if !decodeReq(w, r, &req) {
		return
	}
	if code := validateForwardReq(req.Rule); code != "" {
		writeErr(w, http.StatusBadRequest, code)
		return
	}
	rule := req.Rule.toProtocol()
	s.forwardsMutate(w, r, id, protocol.ForwardsRequest{Action: protocol.ForwardsAdd, Rule: &rule},
		"forward_add", forwardDesc(rule))
}

// handleUpdateNodeForward replaces one forward (delete + add in one nft
// transaction on the probe, so a failure leaves the old rule in place).
func (s *Server) handleUpdateNodeForward(w http.ResponseWriter, r *http.Request) {
	id, ok := s.forwardsNode(w, r)
	if !ok {
		return
	}
	var req forwardUpdateReq
	if !decodeReq(w, r, &req) {
		return
	}
	if code := validateForwardReq(req.Rule); code != "" {
		writeErr(w, http.StatusBadRequest, code)
		return
	}
	old, neu := req.Old.toProtocol(), req.Rule.toProtocol()
	s.forwardsMutate(w, r, id, protocol.ForwardsRequest{Action: protocol.ForwardsUpdate, Old: &old, Rule: &neu},
		"forward_update", forwardDesc(old)+" -> "+forwardDesc(neu))
}

// handleDeleteNodeForward removes one forward and the masquerade rule that
// belongs to it.
func (s *Server) handleDeleteNodeForward(w http.ResponseWriter, r *http.Request) {
	id, ok := s.forwardsNode(w, r)
	if !ok {
		return
	}
	var req forwardDeleteReq
	if !decodeReq(w, r, &req) {
		return
	}
	rule := req.Rule.toProtocol()
	s.forwardsMutate(w, r, id, protocol.ForwardsRequest{Action: protocol.ForwardsDelete, Old: &rule},
		"forward_delete", forwardDesc(rule))
}

// forwardsNode resolves {id} and rejects unknown nodes.
func (s *Server) forwardsNode(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if _, err := s.Store.GetNode(id); !writeStoreErr(w, err) {
		return "", false
	}
	return id, true
}

// forwardsMutate runs one §21 mutation and answers with the resulting state.
// A probe that is offline (or slower than forwardsWait) is not an error: the
// command is queued with its 10-minute TTL and the panel says so.
func (s *Server) forwardsMutate(w http.ResponseWriter, r *http.Request, nodeID string,
	req protocol.ForwardsRequest, auditAction, desc string) {
	res, queued, err := s.forwardsRoundTrip(r, nodeID, req, desc)
	if err != nil {
		s.writeForwardErr(w, err)
		return
	}
	if res != nil {
		// Persist the answer here as well as in the hub: the panel's own response
		// must agree with the next GET, and the state frame may still be in flight.
		s.Hub.RecordForwardState(nodeID, &res.State)
	}
	reply, err := s.forwardsReply(nodeID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal")
		return
	}
	reply.Live = res != nil
	reply.Queued = queued
	if res != nil {
		reply.Warnings = res.Warnings
	}
	s.audit(auditAction, desc, s.Trust.RealIP(r))
	writeJSON(w, http.StatusOK, reply)
}

// forwardErr is a stable error token travelling with a §21 round trip.
type forwardErr struct{ code string }

func (e forwardErr) Error() string { return e.code }

// forwardsRoundTrip enqueues one command and waits a bounded time for the
// agent's structured answer. res is nil when the answer did not arrive in time;
// queued reports that the command is still pending.
func (s *Server) forwardsRoundTrip(r *http.Request, nodeID string, req protocol.ForwardsRequest, reason string) (*protocol.ForwardsResult, bool, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, false, forwardErr{"internal"}
	}
	cmdID, err := s.enqueueCommand(commandEnqueue{
		NodeID: nodeID, Kind: protocol.CmdKindNftForwards, Payload: string(payload),
		Audit: store.AuditEntry{
			Actor: "panel", Reason: reason, Risk: "normal", SourceIP: s.Trust.RealIP(r),
		},
	})
	if err != nil {
		return nil, false, forwardErr{"internal"}
	}
	// Offline probes would burn the whole wait to learn nothing; the queued
	// command is delivered on reconnect (§7).
	if !s.Hub.IsOnline(nodeID) {
		return nil, true, nil
	}
	cmd, ok := s.waitCommandResult(r.Context(), cmdID, forwardsWait)
	if !ok {
		return nil, true, nil
	}
	if cmd.Status == "timeout" {
		return nil, true, nil
	}
	var result protocol.CmdResult
	if err := json.Unmarshal([]byte(cmd.Result), &result); err != nil {
		return nil, false, forwardErr{"forward_failed"}
	}
	// An agent built before §21 answers every unknown kind the same way; that is
	// a version problem, not a user error.
	if strings.Contains(result.Error, "unsupported command kind") {
		return nil, false, forwardErr{"forward_agent_unsupported"}
	}
	var fres protocol.ForwardsResult
	if err := json.Unmarshal([]byte(result.Stdout), &fres); err != nil {
		return nil, false, forwardErr{"forward_failed"}
	}
	if !fres.OK || fres.Error != "" {
		return nil, false, forwardErr{forwardErrorCode(fres.Error)}
	}
	return &fres, false, nil
}

// forwardErrorCode maps the agent's §21 tokens onto the panel's error codes.
// The agent and the server deliberately keep separate vocabularies: the agent's
// is about nftables, the panel's about what the operator can do about it.
func forwardErrorCode(agentCode string) string {
	switch agentCode {
	case "conflict":
		return "forward_conflict"
	case "not_found":
		return "forward_not_found"
	case "ambiguous":
		return "forward_ambiguous"
	case "need_root", "no_permission":
		return "forward_need_root"
	case "nft_missing":
		return "forward_nft_missing"
	case "chain_mismatch":
		return "forward_chain_mismatch"
	case "nft_failed":
		return "forward_failed"
	case "unsupported_os":
		return "forward_unsupported"
	case "bad_comment":
		return "bad_comment"
	case "bad_proto", "bad_port", "bad_ip", "bad_iface", "bad_request", "bad_payload", "bad_action":
		return "bad_forward"
	default:
		return "forward_failed"
	}
}

func (s *Server) writeForwardErr(w http.ResponseWriter, err error) {
	var fe forwardErr
	code := "internal"
	status := http.StatusInternalServerError
	if errors.As(err, &fe) {
		code = fe.code
	}
	switch code {
	case "forward_conflict", "forward_ambiguous":
		status = http.StatusConflict
	case "forward_not_found":
		status = http.StatusNotFound
	case "bad_forward", "bad_comment", "bad_request", "forward_agent_unsupported", "forward_need_root",
		"forward_nft_missing", "forward_chain_mismatch", "forward_unsupported":
		status = http.StatusBadRequest
	case "forward_failed":
		status = http.StatusBadGateway
	}
	writeErr(w, status, code)
}

// forwardViews maps stored rows to the panel view, capped by forwardsMaxRule.
// Shared by the §21 card (whole reply) and the read-only list on the node
// detail page, so both endpoints can never disagree about a rule.
func forwardViews(rows []store.NodeForward) []forwardView {
	views := make([]forwardView, 0, len(rows))
	for i, row := range rows {
		if i >= forwardsMaxRule {
			break
		}
		views = append(views, forwardView{
			Proto: row.Proto, SrcPort: row.SrcPort, Iface: row.Iface,
			DstIP: row.DstIP, DstPort: row.DstPort, Comment: row.Comment,
			Handle: row.Handle, ExtraMatch: row.ExtraMatch,
		})
	}
	return views
}

// forwardsReply builds the panel view from the stored snapshot.
func (s *Server) forwardsReply(nodeID string) (forwardsReply, error) {
	status, err := s.Store.GetNodeForwardStatus(nodeID)
	if err != nil {
		return forwardsReply{}, err
	}
	rows, err := s.Store.ListNodeForwards(nodeID)
	if err != nil {
		return forwardsReply{}, err
	}
	return forwardsReply{
		Supported: status.Supported, Initialized: status.Initialized,
		Code: status.Code, Message: status.Message, ReportedAt: status.ReportedAt,
		AgentSupported: status.ReportedAt > 0,
		Forwards:       forwardViews(rows),
	}, nil
}

// forwardDesc renders a rule for the audit trail.
func forwardDesc(r protocol.ForwardRule) string {
	var b strings.Builder
	b.WriteString(r.Proto + "/" + strconv.Itoa(r.SrcPort))
	if r.Iface != "" {
		b.WriteString("@" + r.Iface)
	}
	b.WriteString(" -> " + r.DstIP + ":" + strconv.Itoa(r.DstPort))
	return b.String()
}
