package watch

import (
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/approvals"
)

const approvalDetailCh = 160

// Approval is one entry of `wave wait`'s `approvals`: a pending item of
// `agent-exec approvals`. Pkg is nil for a network item (a host decision
// covers every package it blocked); Detail is the item's one-line form
// without its id.
type Approval struct {
	ID     string  `json:"id"`
	Kind   string  `json:"kind"`
	Pkg    *string `json:"pkg"`
	Detail string  `json:"detail"`
}

// ApprovalsOf is items as `wave wait` lists them (never nil).
func ApprovalsOf(items []approvals.Item) []Approval {
	out := make([]Approval, 0, len(items))
	for _, it := range items {
		a := Approval{ID: it.ID, Kind: it.Kind}
		if it.Kind == "net" {
			a.Kind = "network"
		} else {
			a.Pkg = ptr(it.Pkg)
		}
		line := strings.TrimSpace(strings.TrimPrefix(approvals.FormatLine(it), it.ID))
		a.Detail = clip(firstLine(line), approvalDetailCh)
		out = append(out, a)
	}
	return out
}
