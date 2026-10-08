package capacity

import "sort"

// Membership is M for one interval: c1 is read before its first sample and c2
// after its last. Tuples lists every tuple bound to the account at c1 or c2;
// Active lists those that could dispatch and therefore need sandwiches.
type Membership struct {
	Holds      bool     `json:"holds"`
	Reasons    []string `json:"reasons,omitempty"`
	AccountRef string   `json:"account_ref,omitempty"`
	Tuples     []string `json:"tuples"`
	Active     []string `json:"active"`
}

// JudgeMembership holds iff both checkpoints are known and share the host
// incarnation, no tuple with an unknown account_ref exists at either, and every
// tuple bound to accountRef at either is identical at both with
// binding_sequence <= c1.sequence. Any change inside fails the whole interval;
// shorter intervals on either side of it are judged on their own.
func JudgeMembership(accountRef string, c1, c2 Checkpoint) Membership {
	m := Membership{AccountRef: accountRef, Tuples: []string{}, Active: []string{}}
	add := func(r string) { m.Reasons = append(m.Reasons, r) }
	switch {
	case c1.Coverage != "known" || c2.Coverage != "known":
		add("inventory_unknown")
	case c1.Incarnation == "" || c1.Incarnation != c2.Incarnation:
		add("inventory_incarnation_changed")
	case c2.Sequence < c1.Sequence:
		add("inventory_sequence_regressed")
	}
	if len(m.Reasons) > 0 {
		return m
	}
	if accountRef == "" {
		add("account_ref_unknown")
	}
	at1, at2 := map[string]Tuple{}, map[string]Tuple{}
	bound := map[string]bool{}
	for _, side := range []struct {
		c  Checkpoint
		at map[string]Tuple
	}{{c1, at1}, {c2, at2}} {
		for _, t := range side.c.Tuples {
			side.at[t.Ref] = t
			if t.AccountRef == "" {
				add("unknown_account_ref_tuple")
			} else if t.AccountRef == accountRef {
				bound[t.Ref] = true
			}
		}
	}
	for ref := range bound {
		m.Tuples = append(m.Tuples, ref)
	}
	sort.Strings(m.Tuples)
	for _, ref := range m.Tuples {
		x, ok1 := at1[ref]
		y, ok2 := at2[ref]
		if !ok1 || !ok2 || x != y || x.BindingSequence > c1.Sequence {
			add("binding_changed_inside")
		}
		if ok1 && x.State == "active" || ok2 && y.State == "active" {
			m.Active = append(m.Active, ref)
		}
	}
	if len(m.Tuples) == 0 {
		add("no_account_tuples")
	}
	m.Reasons = unique(m.Reasons)
	m.Holds = len(m.Reasons) == 0
	return m
}

func unique(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	out := append([]string(nil), s...)
	sort.Strings(out)
	n := 0
	for _, x := range out {
		if n == 0 || out[n-1] != x {
			out[n] = x
			n++
		}
	}
	return out[:n]
}
