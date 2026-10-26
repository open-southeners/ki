package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/open-southeners/ki-agent/internal/auth"
	"github.com/open-southeners/ki-agent/internal/rpc"
	"github.com/open-southeners/ki-agent/internal/store"
)

// Audit list paging.
const (
	defaultAuditLimit = 100
	maxAuditLimit     = 500
)

// AuditEntry is the wire form of an audit log row.
type AuditEntry struct {
	ID            int64     `json:"id"`
	Time          time.Time `json:"time"`
	PrincipalKind string    `json:"principal_kind"`
	PrincipalName string    `json:"principal_name"`
	IP            string    `json:"ip"`
	Action        string    `json:"action"`
	Target        string    `json:"target"`
	Result        string    `json:"result"`
}

func (s *Service) audit(ctx context.Context, kind, name, ip, event, target, result string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	e := &store.AuditEntry{Time: s.now(), PrincipalKind: kind, PrincipalName: name, IP: ip,
		Event: event, Target: target, Result: result}
	if err := s.st.AppendAudit(ctx, e); err != nil {
		s.log.Warn("append audit", "event", event, "err", err)
	}
}

// ListAudit returns up to limit entries (default 100, at most 500), newest
// first, older than beforeID when it is positive.
func (s *Service) ListAudit(ctx context.Context, limit int, beforeID int64) ([]AuditEntry, error) {
	if limit < 0 || beforeID < 0 {
		return nil, invalidf("limit and before_id must not be negative")
	}
	if limit == 0 {
		limit = defaultAuditLimit
	}
	limit = min(limit, maxAuditLimit)
	es, err := s.st.ListAudit(ctx, limit, beforeID)
	if err != nil {
		return nil, err
	}
	out := make([]AuditEntry, 0, len(es))
	for _, e := range es {
		out = append(out, AuditEntry{ID: e.ID, Time: e.Time, PrincipalKind: e.PrincipalKind, PrincipalName: e.PrincipalName,
			IP: e.IP, Action: e.Event, Target: e.Target, Result: e.Result})
	}
	return out, nil
}

// targetKeys are the only params ever copied into the audit log, in priority order.
var targetKeys = []string{"slug", "project", "username", "name", "id"}

func auditTarget(params json.RawMessage) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(params, &m) != nil {
		return ""
	}
	for _, k := range targetKeys {
		raw, ok := m[k]
		if !ok {
			continue
		}
		var str string
		if json.Unmarshal(raw, &str) == nil {
			return clip(str)
		}
		var n json.Number
		if json.Unmarshal(raw, &n) == nil {
			return n.String()
		}
	}
	return ""
}

func resultOf(err error) string {
	if err == nil {
		return "ok"
	}
	var re *rpc.Error
	if errors.As(err, &re) {
		return strconv.Itoa(re.Code)
	}
	return strconv.Itoa(rpc.CodeInternalError)
}

// AuditMiddleware records every call to a method that needs the operator or
// admin role: who, from where, which method, a target taken from a fixed set of
// params (slug, project, username, name, id) and the outcome. Install it inside
// the authorisation middleware so calls denied by the role check are not
// recorded as performed.
func (s *Service) AuditMiddleware() rpc.Middleware {
	return func(method string, next rpc.Handler) rpc.Handler {
		need, ok := auth.MethodRoles[method]
		if !ok || need < auth.RoleOperator {
			return next
		}
		return func(ctx context.Context, p *rpc.Peer, params json.RawMessage) (any, error) {
			res, err := next(ctx, p, params)
			pr, _ := auth.PrincipalOf(p)
			s.audit(ctx, pr.Kind, pr.Name, auth.ClientIPOf(p), method, auditTarget(params), resultOf(err))
			return res, err
		}
	}
}
